package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/tools"
)

// The session tools are the ONLY way anything reaches the supervisor.
//
// One surface, not three: the CLI drives them through `karmax tool call`, a
// workflow lends them to its gateway, and the orchestrator uses them directly.
// Adding an API endpoint and a cobra command and a loopkit method for the same
// capability is how three subtly different behaviours end up in one system.
//
// Nothing here knows what a session is FOR. The key is the caller's; a workflow
// that wants a session per WhatsApp chat passes "chat:<jid>" and core never
// learns what that means.

// harnessRef is a late-bound pointer to the runtime.
//
// The tools must be registered before the agents are built, because an agent
// binds its toolset at construction and never looks at the registry again. The
// runtime does not exist until after that. So the tools are created early
// holding this, and it is filled in once there is something to point at.
type harnessRef struct{ rt *KarmaxRuntime }

func (h *harnessRef) get() *KarmaxRuntime {
	if h == nil {
		return nil
	}
	return h.rt
}

type harnessSendTool struct{ ref *harnessRef }

func (t *harnessSendTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "harness.send",
		Description: "Send a message to a long-lived Claude Code session and get its reply. " +
			"Sessions are keyed by an arbitrary string: the same key continues the same conversation, " +
			"a new key starts a new one. Opening, resuming after a crash and closing when idle are automatic. " +
			"Pick the model for the job: haiku for cheap classification and summarising, sonnet for ordinary " +
			"work, opus for hard reasoning, fable for the hardest. Omit it to use the kind's default.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"key":{"type":"string","description":"Stable handle for this conversation, e.g. \"chat:<jid>\" or \"task:build-apk\"."},
				"kind":{"type":"string","description":"Which configured policy to use (model, idle window, limits). Defaults to \"agent\"."},
				"text":{"type":"string","description":"The message to send."},
				"model":{"type":"string","description":"Override the kind's model: haiku | sonnet | opus | fable, or a full model id. Takes effect when the session is opened or resumed, not mid-conversation."},
				"workdir":{"type":"string","description":"Directory the session runs in. It can read and write here, and inherits any CLAUDE.md above it."},
				"instructions":{"type":"string","description":"Standing brief written to CLAUDE.md in the workdir before the first spawn. Seeded once; the session owns the file afterwards."}
			},
			"required":["key","text"]
		}`),
	}
}

func (t *harnessSendTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	rt := t.ref.get()
	if rt == nil || rt.harness == nil {
		return tools.ErrorResult(fmt.Errorf("the harness is not enabled (set harness.enabled in karmax.yaml)")), nil
	}
	sup := rt.harness
	key, _ := in["key"].(string)
	text, _ := in["text"].(string)
	kind, _ := in["kind"].(string)
	if strings.TrimSpace(key) == "" || strings.TrimSpace(text) == "" {
		return tools.ErrorResult(fmt.Errorf("key and text are required")), nil
	}
	if strings.TrimSpace(kind) == "" {
		kind = "agent"
	}

	model, _ := in["model"].(string)
	workdir, _ := in["workdir"].(string)
	instructions, _ := in["instructions"].(string)

	turn, err := sup.SendWith(ctx, key, kind, text, harness.Options{
		Model:        strings.TrimSpace(model),
		Workdir:      strings.TrimSpace(workdir),
		Instructions: instructions,
	})
	if err != nil {
		// A tripped breaker is not a failure of this call — it is the system
		// telling the caller to use its other path. Saying so plainly is what
		// lets a workflow fall back instead of surfacing an error to a person.
		var open harness.ErrBreakerOpen
		if asBreakerOpen(err, &open) {
			return tools.SuccessResult(map[string]any{
				"available": false,
				"reason":    open.Reason,
				"advice":    "use the API path for this turn",
			}), nil
		}
		return tools.ErrorResult(err), nil
	}

	used := make([]string, 0, len(turn.ToolCalls))
	for _, tc := range turn.ToolCalls {
		used = append(used, tc.Name)
	}
	return tools.SuccessResult(map[string]any{
		"available":  true,
		"reply":      turn.Text,
		"tools_used": used,
		"cost_usd":   turn.CostUSD,
		"usage": map[string]any{
			"input": turn.Usage.InputTokens, "output": turn.Usage.OutputTokens,
			"cache_read": turn.Usage.CacheReadTokens,
		},
	}), nil
}

type harnessListTool struct{ ref *harnessRef }

func (t *harnessListTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "harness.list",
		Description: "List harness sessions: which are live, what they have cost, and how much of the account's rate-limit windows is gone.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (t *harnessListTool) Execute(ctx context.Context, _ map[string]any) (tools.ToolResult, error) {
	rt := t.ref.get()
	if rt == nil || rt.harness == nil {
		return tools.SuccessResult(map[string]any{"enabled": false}), nil
	}
	sup := rt.harness
	rows, err := rt.store.ListHarnessSessions()
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	live := map[string]bool{}
	for _, k := range sup.Live() {
		live[k] = true
	}

	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"key": r.Key, "kind": r.Kind, "model": r.Model,
			"state": r.State, "live": live[r.Key], "turns": r.Turns,
			"cost_usd": r.CostUSD, "cache_read": r.CacheRead,
			"idle_for":   time.Since(r.LastActivityAt).Round(time.Second).String(),
			"last_error": r.LastError,
		})
	}

	res := map[string]any{"enabled": true, "sessions": out}
	if rt.harnessBreaker != nil {
		tripped, reason, rl := rt.harnessBreaker.Status()
		res["paused"] = tripped
		res["pause_reason"] = reason
		if rl != nil {
			windows := map[string]any{}
			for name, w := range rl.UnifiedWindows {
				windows[name] = map[string]any{
					"used":      fmt.Sprintf("%.0f%%", w.Utilization*100),
					"resets_at": time.Unix(w.ResetsAt, 0).Format(time.RFC3339),
				}
			}
			res["rate_limits"] = windows
		}
	}
	return tools.SuccessResult(res), nil
}

type harnessCloseTool struct{ ref *harnessRef }

func (t *harnessCloseTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "harness.close",
		Description: "Close a harness session now, rather than waiting for its idle window.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{"key":{"type":"string","description":"The session key to close."}},
			"required":["key"]
		}`),
	}
}

func (t *harnessCloseTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	rt := t.ref.get()
	if rt == nil || rt.harness == nil {
		return tools.ErrorResult(fmt.Errorf("the harness is not enabled")), nil
	}
	key, _ := in["key"].(string)
	if strings.TrimSpace(key) == "" {
		return tools.ErrorResult(fmt.Errorf("key is required")), nil
	}
	rt.harness.Close(key)
	return tools.SuccessResult(map[string]any{"closed": key}), nil
}

func asBreakerOpen(err error, out *harness.ErrBreakerOpen) bool {
	if e, ok := err.(harness.ErrBreakerOpen); ok {
		*out = e
		return true
	}
	return false
}
