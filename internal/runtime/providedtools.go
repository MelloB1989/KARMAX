package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/wasmloop"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// Lending a workflow's tools to the agent.
//
// A workflow's provided tools are NOT part of the agent's permanent toolset.
// They exist while the agent is working on that workflow's behalf and nowhere
// else, for two reasons: a hundred installed workflows would otherwise put a
// hundred tools in every prompt, and a tool that is only meaningful in the
// middle of one workflow's job is noise everywhere else.
//
// "On that workflow's behalf" means two things, and they need different
// machinery:
//
//   - The ask() or gateway() the workflow made itself. Direct: the call site
//     knows which loop it came from.
//   - A later turn CAUSED by the workflow — the delegation.completed event for
//     a background job it started. Indirect: by then the workflow's run has
//     returned, so the link has to have been recorded when the job began.
//
// attributions is that record.

// providedToolTimeout bounds one call into a workflow. Generous, because the
// handler may itself call back out to the host for memory or an integration.
const providedToolTimeout = 60 * time.Second

// providedTool exposes one of a workflow's tools as an ordinary tools.Tool.
type providedTool struct {
	loop   string
	spec   wasmloop.ToolSpec
	runner *wasmloop.Runner
	log    *zap.Logger
}

func (t *providedTool) Manifest() tools.ToolManifest {
	params := t.spec.Parameters
	if len(params) == 0 {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return tools.ToolManifest{
		Name: t.spec.Name,
		// Attributed to its workflow. The model is choosing between this and
		// the built-ins, and "who provides this" is what tells it whether the
		// answer is authoritative or one loop's opinion.
		Description: t.spec.Description + " (provided by the " + t.loop + " workflow)",
		Parameters:  params,
	}
}

func (t *providedTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	out, err := t.runner.InvokeTool(ctx, t.spec.Name, input, providedToolTimeout)
	if err != nil {
		t.log.Warn("a workflow failed to serve the tool it provides",
			zap.String("loop", t.loop), zap.String("tool", t.spec.Name), zap.Error(err))
		// Surfaced to the model rather than aborting the turn: a workflow being
		// broken should cost that answer, not the conversation.
		return tools.ErrorResult(err), nil
	}
	return tools.SuccessResult(map[string]any{"output": out}), nil
}

// providedTools returns the tools a workflow lends, gated by the Broker.
//
// Gated on the CALLER's subject, not the workflow's: the question is whether
// this agent may use the tool, and a workflow cannot widen its own reach by
// declaring a tool the operator never approved.
func (rt *KarmaxRuntime) providedTools(loop string) []tools.Tool {
	rt.providedMu.RLock()
	runner, ok := rt.wasmByName[loop]
	rt.providedMu.RUnlock()
	if !ok || runner == nil {
		return nil
	}

	grants := rt.broker.For(broker.LoopSubject(loop))
	var out []tools.Tool
	for _, spec := range runner.ProvidedTools() {
		if strings.TrimSpace(spec.Name) == "" {
			continue
		}
		if err := grants.Check("tool", "provides:"+spec.Name); err != nil {
			rt.log.Warn("a workflow's provided tool was not granted, so it is not offered",
				zap.String("loop", loop), zap.String("tool", spec.Name))
			continue
		}
		out = append(out, &providedTool{loop: loop, spec: spec, runner: runner, log: rt.log})
	}
	return out
}

// attributions records which workflow caused a piece of work, so a turn that
// arrives later can still be traced back to it.
//
// Bounded and swept: an entry that is never claimed — a job that died, a
// delegation that never completed — must not keep a workflow's tools alive in
// the agent's context forever.
type attributions struct {
	mu    sync.Mutex
	byKey map[string]attribution
}

type attribution struct {
	loop string
	at   time.Time
}

const attributionTTL = 6 * time.Hour

func newAttributions() *attributions {
	return &attributions{byKey: map[string]attribution{}}
}

// Record links a cause — a job id, a delegation id — to the workflow behind it.
func (a *attributions) Record(key, loop string) {
	if key == "" || loop == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweep()
	a.byKey[key] = attribution{loop: loop, at: time.Now()}
}

// Loop returns the workflow behind a cause, if it is still known.
func (a *attributions) Loop(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	at, ok := a.byKey[key]
	if !ok || time.Since(at.at) > attributionTTL {
		return "", false
	}
	return at.loop, true
}

// Forget drops a cause once its work has been accounted for.
func (a *attributions) Forget(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.byKey, key)
}

func (a *attributions) sweep() {
	for k, v := range a.byKey {
		if time.Since(v.at) > attributionTTL {
			delete(a.byKey, k)
		}
	}
}

// attributeJobs records the background work a lent turn kicked off.
//
// A tool that starts something in the background answers with a job id NOW and
// publishes the outcome as an event minutes later. Without this the later turn
// has no idea a workflow is behind it, and the tools it needs to act on the
// result are gone.
func (rt *KarmaxRuntime) attributeJobs(loop string, calls []karmahelper.ToolCallRecord) {
	for _, c := range calls {
		out, ok := c.Result.Output.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := out["job_id"].(string); id != "" {
			rt.attributions.Record(id, loop)
			rt.log.Debug("attributed background work to a workflow",
				zap.String("loop", loop), zap.String("job", id), zap.String("tool", c.Name))
		}
	}
}

// lentToolsForEvent decides which provided tools a turn gets.
//
// Two ways an event belongs to a workflow: it IS that workflow's trigger, or it
// is the completion of work the workflow started.
func (rt *KarmaxRuntime) lentToolsForEvent(evt bus.Event) []tools.Tool {
	if evt.Payload != nil {
		if id, _ := evt.Payload["job_id"].(string); id != "" {
			if loop, ok := rt.attributions.Loop(id); ok {
				// Claimed once. The work is done, so holding the link only
				// risks lending a stale workflow's tools to a later job that
				// happens to reuse the id.
				rt.attributions.Forget(id)
				return rt.providedTools(loop)
			}
		}
		if loop, _ := evt.Payload["loopkit"].(string); loop != "" {
			return rt.providedTools(loop)
		}
	}
	return nil
}

// describeProvided renders a workflow's provided tools for an install preview.
func describeProvided(specs []wasmloop.ToolSpec) string {
	if len(specs) == 0 {
		return ""
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return fmt.Sprintf("provides %d tool(s): %s", len(specs), strings.Join(names, ", "))
}
