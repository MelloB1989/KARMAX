package runtime

import (
	"encoding/json"
	"testing"

	"github.com/MelloB1989/karmax/internal/harness"
)

// toolUpdate builds the streaming event a tool_result produces: the only
// shape ticketFrom ever looks at.
func toolUpdate(id, output string, status harness.Status) harness.Event {
	return harness.Event{
		Kind: harness.KindToolUpdate,
		Tool: &harness.ToolEvent{ID: id, Status: status, Output: output, Title: "fallback title"},
	}
}

// A background job's result — whatever tool returned it, under whatever
// harness tool name it arrived as (task.start, claude_code.call
// --background and sandbox.start all look different to the harness) —
// becomes a ticket carrying its id and a label drawn from the same JSON.
func TestATaskStartResultEmitsATicketWithItsID(t *testing.T) {
	e := toolUpdate("call-1", `{"status":"started","task_id":"tsk-42","goal":"back up the drive"}`, harness.StatusCompleted)
	ev, ok := ticketFrom(e, map[string]bool{})
	if !ok {
		t.Fatalf("ticketFrom returned no ticket for a task.start result")
	}
	if ev.Kind != "ticket" || ev.JobID != "tsk-42" {
		t.Fatalf("ticket = %+v, want Kind=ticket JobID=tsk-42", ev)
	}
	if ev.Text != "back up the drive" {
		t.Fatalf("ticket text = %q, want the goal field", ev.Text)
	}
}

// tool_update can arrive more than once for the same call; the card must
// not double.
func TestATicketIsEmittedOncePerToolCall(t *testing.T) {
	e := toolUpdate("call-1", `{"job_id":"job-9"}`, harness.StatusCompleted)
	seen := map[string]bool{}
	if _, ok := ticketFrom(e, seen); !ok {
		t.Fatalf("first tool_update for this call should have produced a ticket")
	}
	if _, ok := ticketFrom(e, seen); ok {
		t.Fatalf("a second tool_update for the same call produced a second ticket")
	}
}

// A tool result with no task/job/run id is just a tool result — most of
// them are, and none of those are tickets.
func TestAToolResultWithoutATaskIDEmitsNoTicket(t *testing.T) {
	e := toolUpdate("call-1", `{"status":"completed","output":"done"}`, harness.StatusCompleted)
	if _, ok := ticketFrom(e, map[string]bool{}); ok {
		t.Fatalf("a result with no id must not become a ticket")
	}
}

// truncateOutput can cut a result's JSON mid-object; that must read as "no
// ticket found", never crash the turn.
func TestUnparseableToolOutputEmitsNoTicketAndDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ticketFrom panicked on truncated output: %v", r)
		}
	}()
	e := toolUpdate("call-1", `{"status":"started","task_id":"tsk-1`, harness.StatusCompleted)
	if _, ok := ticketFrom(e, map[string]bool{}); ok {
		t.Fatalf("truncated JSON must not produce a ticket")
	}
}

// When the tool's own result carries no goal/title/prompt label, the card
// falls back to the tool's title rather than going out blank.
func TestATicketFallsBackToTheToolsTitleWhenNoLabelField(t *testing.T) {
	e := toolUpdate("call-1", `{"job_id":"job-1"}`, harness.StatusCompleted)
	ev, ok := ticketFrom(e, map[string]bool{})
	if !ok {
		t.Fatalf("expected a ticket")
	}
	if ev.Text != "fallback title" {
		t.Fatalf("ticket text = %q, want the tool's own title", ev.Text)
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

// An empty (or nil) plan must marshal to "[]", not "null": the TypeScript
// side declares plan non-nullable and reads plan.length unguarded, so a null
// would throw inside deriveRows and take the whole transcript down with it.
func TestAPIChatPlanEmptyMarshalsAsEmptyArray(t *testing.T) {
	for name, entries := range map[string][]harness.PlanEntry{"nil": nil, "empty": {}} {
		b, err := json.Marshal(apiChatPlan(entries))
		if err != nil {
			t.Fatalf("%s: marshal error: %v", name, err)
		}
		if string(b) != "[]" {
			t.Fatalf("%s: plan JSON = %s, want []", name, b)
		}
	}
}
