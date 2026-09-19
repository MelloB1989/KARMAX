package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
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
	// Deliberately unconditional, not CloseIfIdle: this is the operator (or
	// an agent acting for them) explicitly asking to close key right now,
	// which is exactly the case where killing a busy turn is the intended
	// behaviour — "close it now" loses its meaning if it silently no-ops
	// whenever there happens to be a turn in flight. Session.Close's own
	// writer synchronization (see session.go) means this can no longer
	// corrupt the session it interrupts; it can only interrupt it, which is
	// what was asked for.
	rt.harness.Close(key)
	return tools.SuccessResult(map[string]any{"closed": key}), nil
}

// harnessStopTool is the desktop app's Stop button: it targets a claude_code
// run in flight (the lyzn-tasks recipe's "harness:" step, keyed by
// session_id — see loophost.go's HarnessWith/HarnessForget), not a
// harness.send/harness.list/harness.close conversation. Those live on
// rt.harness, the Supervisor; this one reaches into
// internal/tools/builtin's own package-level registry of in-flight CLI
// calls, because ClaudeCodeTool is built fresh on every call and has
// nowhere else to keep that state — see claude_code_runs.go.
type harnessStopTool struct{ ref *harnessRef }

func (t *harnessStopTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "harness.stop",
		Description: "Stop a claude_code run in progress and block that key from starting a new one for 30 " +
			"minutes. Cancels the whole process group the CLI started — not just its direct process — then " +
			"cleans up its transcript, its session-key mapping and its working directory, the same as " +
			"harness.forget. Safe to call when nothing is running under the key.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"session_id":{"type":"string","description":"The session_id (or session key, e.g. \"lyzn:<task id>\") exactly as it was passed to claude_code.call."},
				"working_dir":{"type":"string","description":"Working directory to clean up when nothing is currently running under this key. Ignored when a run was in flight — its own resolved working directory is used instead."}
			},
			"required":["session_id"]
		}`),
	}
}

func (t *harnessStopTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	rt := t.ref.get()
	if rt == nil {
		return tools.ErrorResult(fmt.Errorf("the runtime is not ready")), nil
	}
	sessionID, _ := in["session_id"].(string)
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return tools.ErrorResult(fmt.Errorf("session_id is required")), nil
	}
	givenDir, _ := in["working_dir"].(string)

	// Bounded, not indefinite: a run that ignores every signal must not
	// hang the operator's Stop button forever. terminateGroup's own
	// SIGKILL escalation (5s) has already fired well within this by the
	// time it would matter.
	wasRunning, resolvedDir := builtin.StopRun(sessionID, 10*time.Second)

	dir := resolvedDir
	if !wasRunning {
		dir = strings.TrimSpace(givenDir)
	}

	// Nothing to clean up without a directory: this is exactly the "stop
	// with nothing running, and no working_dir given" case, and it must
	// not be an error.
	if dir != "" {
		dataDir := ""
		if rt.cfg != nil {
			dataDir = rt.cfg.Karmax.DataDir
		}
		cleaner := &builtin.ClaudeCodeTool{Store: rt.store, DataDir: dataDir}
		if err := cleaner.Cleanup(dir, sessionID); err != nil {
			return tools.ErrorResult(err), nil
		}
	}

	return tools.SuccessResult(map[string]any{
		"session_id":  sessionID,
		"was_running": wasRunning,
		"working_dir": dir,
	}), nil
}

func asBreakerOpen(err error, out *harness.ErrBreakerOpen) bool {
	if e, ok := err.(harness.ErrBreakerOpen); ok {
		*out = e
		return true
	}
	return false
}
