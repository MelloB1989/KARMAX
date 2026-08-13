package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/google/uuid"
)

// Handing work to a copy of yourself.
//
// The agent can already delegate to a coding harness, which is the right home
// for anything needing a shell or a repository. What it could not do is fan out:
// take three independent questions and answer them at once, each in its own
// context, without the parent's conversation dragging along behind.
//
// A child gets a written brief and nothing else — not the parent's history, not
// its pending questions. That is the constraint that makes this cheap: the
// parent's context stays small precisely because the work left it.

// SpawnFunc runs a task on a fresh agent instance and returns its answer. The
// runtime supplies it; this package must not know how an agent is constructed.
//
// grant is the toolset the child runs with. Empty means "inherit mine", which
// is the ordinary case; naming tools builds a child around a capability the
// orchestrator itself does not carry, which is how a small orchestrator reaches
// a large instance without holding all of it.
type SpawnFunc func(ctx context.Context, childID, brief string, grant []tools.Tool) (string, error)

// Limits on fanning out. Deliberately small: every child is a full model
// conversation, and an agent that can spawn without bound can spend without
// bound.
const (
	maxConcurrentChildren = 4
	maxChildDepth         = 2
	childTimeout          = 10 * time.Minute
)

// SubagentTool lets the agent run tasks on copies of itself.
type SubagentTool struct {
	Store   *store.Store
	AgentID string
	Spawn   SpawnFunc
	Publish func(bus.Event) error
	// Registry resolves the tool names a spawn asks for. The whole instance,
	// not the orchestrator's own set: the point of naming tools is to give a
	// child something the orchestrator does not carry.
	Registry *tools.Registry
	// Depth is how deep this agent already is. A child spawned by a child
	// inherits depth+1, which is what stops a fan-out from recursing.
	Depth int
}

func (t *SubagentTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "subagent.spawn",
		Description: "Hand a self-contained task to a copy of yourself and get its answer back. " +
			"Use this to work on several independent things at once — researching three questions, checking four chats, " +
			"drafting and reviewing in parallel — where each piece can be done without the others. " +
			"The copy sees ONLY the brief you write: it has none of your conversation, so the brief must name the " +
			"chat, person, file or project it needs. For anything wanting a shell, a repo or the web, use claude_code.call instead.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"tasks": {
					"type": "array",
					"items": {"type": "string"},
					"description": "One complete, self-contained brief per copy. Each must stand alone."
				},
				"background": {
					"type": "boolean",
					"description": "true to return immediately and receive each result as an event later. Use for slow work; the default waits."
				},
				"tools": {
					"type": "array",
					"items": {"type": "string"},
					"description": "Exact tool names to build the child around, e.g. [\"linkedin.post\"]. Use this to give a child a capability you do not carry yourself — karmax.capabilities lists what exists on this instance. Omit to give the child your own toolset."
				}
			},
			"required": ["tasks"]
		}`),
	}
}

func (t *SubagentTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	if t.Spawn == nil {
		return tools.ErrorResult(fmt.Errorf("sub-agents are not available on this instance")), nil
	}
	if t.Depth >= maxChildDepth {
		return tools.ErrorResult(fmt.Errorf(
			"already %d levels deep; do this work yourself rather than delegating further", t.Depth)), nil
	}

	tasks := stringList(input["tasks"])
	if len(tasks) == 0 {
		return tools.ErrorResult(fmt.Errorf("give at least one task")), nil
	}

	running, err := t.Store.RunningSubagents(t.AgentID)
	if err == nil && running+len(tasks) > maxConcurrentChildren {
		return tools.ErrorResult(fmt.Errorf(
			"that would run %d copies at once and the limit is %d — send fewer, or wait for the ones already working",
			running+len(tasks), maxConcurrentChildren)), nil
	}

	grant, err := t.resolveGrant(stringList(input["tools"]))
	if err != nil {
		return tools.ErrorResult(err), nil
	}

	background, _ := input["background"].(bool)
	if background && t.Publish == nil {
		background = false
	}

	if background {
		for _, task := range tasks {
			t.start(task, true, grant)
		}
		return tools.SuccessResult(map[string]any{
			"status": "started",
			"count":  len(tasks),
			"note":   "each copy reports back as a delegation.completed event when it finishes",
		}), nil
	}

	// Foreground: run them together and return every answer. Concurrent because
	// the whole reason to spawn is that these do not depend on each other.
	type result struct {
		Task   string `json:"task"`
		Answer string `json:"answer,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	out := make([]result, len(tasks))
	done := make(chan struct{}, len(tasks))
	for i, task := range tasks {
		go func(i int, task string) {
			defer func() { done <- struct{}{} }()
			answer, err := t.run(ctx, task, grant)
			out[i] = result{Task: truncate(task, 120), Answer: answer}
			if err != nil {
				out[i].Error = err.Error()
			}
		}(i, task)
	}
	for range tasks {
		select {
		case <-done:
		case <-ctx.Done():
			return tools.ErrorResult(ctx.Err()), nil
		}
	}
	return tools.SuccessResult(map[string]any{"results": out}), nil
}

// start launches a child in the background, detached from the caller's turn.
func (t *SubagentTool) start(task string, announce bool, grant []tools.Tool) {
	go func() {
		// Not the turn's context: the turn ends long before this does, which is
		// the entire point of running it in the background.
		ctx, cancel := context.WithTimeout(context.Background(), childTimeout)
		defer cancel()

		answer, err := t.run(ctx, task, grant)
		if !announce || t.Publish == nil {
			return
		}
		payload := map[string]any{
			"job_id": uuid.New().String(),
			"tool":   "subagent.spawn",
			"task":   truncate(task, 500),
		}
		if err != nil {
			payload["status"], payload["error"] = "failed", err.Error()
		} else {
			payload["status"], payload["output"] = "completed", answer
		}
		_ = t.Publish(bus.NewEvent(bus.EventDelegationDone, t.AgentID, payload))
	}()
}

// run spawns one child, recording it before the work starts so a crash leaves a
// trace rather than a job nobody knows ran.
func (t *SubagentTool) run(ctx context.Context, task string, grant []tools.Tool) (string, error) {
	runID := uuid.New().String()
	childID := fmt.Sprintf("%s/sub/%s", t.AgentID, runID[:8])

	if t.Store != nil {
		if err := t.Store.StartSubagentRun(store.SubagentRun{
			ID: runID, ParentID: t.AgentID, ChildID: childID,
			Task: truncate(task, 2000), Depth: t.Depth + 1,
		}); err != nil {
			return "", fmt.Errorf("could not record the sub-agent run: %w", err)
		}
	}

	answer, err := t.Spawn(ctx, childID, brief(task), grant)
	if t.Store != nil {
		status, result := "ok", truncate(answer, 4000)
		if err != nil {
			status, result = "failed", err.Error()
		}
		_ = t.Store.FinishSubagentRun(runID, status, result)
	}
	return answer, err
}

// brief wraps the task with what a copy needs to know about its own situation.
// Without this a child behaves like the parent — asking follow-up questions of
// an operator who is not listening to it.
func brief(task string) string {
	return "You are a focused copy of the agent, running one task and reporting back.\n\n" +
		"You do NOT share the parent's conversation, and nobody is reading your output as it happens: " +
		"there is no one to ask a follow-up question of. Work from what is written here, use your tools to " +
		"find what you need, and finish with the answer itself — not a plan to produce it.\n\n" +
		"A tool that refuses you is usually telling you how to succeed: a guard that names what it " +
		"objected to, an argument it wants differently. Read what it said, change that, and call it again " +
		"before concluding the task cannot be done. Two or three attempts, not one, and not twenty.\n\n" +
		"If you genuinely cannot complete it, say exactly what is missing.\n\n" +
		"## Your task\n\n" + strings.TrimSpace(task)
}

// resolveGrant turns requested tool names into the child's toolset.
//
// Resolved against the whole instance rather than the orchestrator's own set,
// which is the point: the orchestrator stays small and reaches a capability by
// building a child around it, instead of carrying every tool it might one day
// need. An unknown name is refused rather than dropped — a child quietly
// missing the one tool its brief depends on produces a confident, useless
// answer, which is worse than a spawn that failed.
func (t *SubagentTool) resolveGrant(names []string) ([]tools.Tool, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if t.Registry == nil {
		return nil, fmt.Errorf("this instance cannot grant tools to a sub-agent; omit 'tools' to give it yours")
	}

	out := make([]tools.Tool, 0, len(names))
	var unknown []string
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		tool, ok := t.Registry.Get(name)
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		// No spawning from a spawn, however it is asked for.
		if tools.CanonicalName(tool.Manifest().Name) == "subagent_spawn" {
			continue
		}
		out = append(out, tool)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("no tool named %s on this instance — call karmax.capabilities to see what exists",
			strings.Join(unknown, ", "))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("none of those names resolved to a usable tool")
	}
	return out, nil
}
