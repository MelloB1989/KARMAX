package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/harness"
)

func toolCall(name string, in map[string]any) harness.ToolCall {
	b, _ := json.Marshal(in)
	return harness.ToolCall{Name: name, Input: b}
}

// Only a background claude_code.call or codex.call becomes a ticket; a
// foreground delegation is watched to completion and needs no card, and a
// tool outside that pair is not a delegation at all.
func TestChatTicketsOnlyBackgroundDelegations(t *testing.T) {
	longPrompt := strings.Repeat("x", 120)
	turn := harness.Turn{ToolCalls: []harness.ToolCall{
		toolCall("claude_code.call", map[string]any{"background": true, "job_id": "job-1", "prompt": longPrompt}),
		toolCall("claude_code.call", map[string]any{"background": false, "job_id": "job-2", "prompt": "short"}),
		toolCall("codex.call", map[string]any{"background": true, "job_id": "job-3", "prompt": "short"}),
		toolCall("shell.exec", map[string]any{"background": true, "job_id": "job-4", "prompt": "short"}),
	}}

	got := chatTickets(turn)
	if len(got) != 2 {
		t.Fatalf("tickets = %d, want 2: %+v", len(got), got)
	}
	if got[0].Kind != "ticket" || got[0].JobID != "job-1" {
		t.Fatalf("first ticket = %+v", got[0])
	}
	if !strings.HasSuffix(got[0].Text, "…") || len(got[0].Text) > 84 {
		t.Fatalf("long prompt was not trimmed to 80 chars: %q", got[0].Text)
	}
	if got[1].JobID != "job-3" || got[1].Text != "short" {
		t.Fatalf("second ticket = %+v", got[1])
	}
}

// A job with no background flag set at all is not a delegation the client
// needs to wait on.
func TestChatTicketsIgnoresUnparseableInput(t *testing.T) {
	turn := harness.Turn{ToolCalls: []harness.ToolCall{
		{Name: "claude_code.call", Input: json.RawMessage(`not json`)},
	}}
	if got := chatTickets(turn); len(got) != 0 {
		t.Fatalf("tickets = %+v, want none", got)
	}
}

// A known kind passes through unchanged; an ACP client's own vocabulary
// (nothing the harness emits today, but the next thing to wire in) must not
// leak past the TypeScript client's closed union.
func TestAPIToolKindCoercesUnknown(t *testing.T) {
	if got := apiToolKind(harness.ToolEdit); got != "edit" {
		t.Fatalf("known kind = %q, want %q", got, "edit")
	}
	if got := apiToolKind(harness.ToolKind("browse")); got != "other" {
		t.Fatalf("unknown kind = %q, want %q", got, "other")
	}
}

// A known status passes through unchanged; an unrecognised one is reported as
// failed, not completed — we do not know it succeeded.
func TestAPIToolStatusCoercesUnknown(t *testing.T) {
	if got := apiToolStatus(harness.StatusCompleted); got != "completed" {
		t.Fatalf("known status = %q, want %q", got, "completed")
	}
	if got := apiToolStatus(harness.Status("cancelled")); got != "failed" {
		t.Fatalf("unknown status = %q, want %q", got, "failed")
	}
}
