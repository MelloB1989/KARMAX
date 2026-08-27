package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
)

func TestWaiterMatches(t *testing.T) {
	payload := map[string]any{
		"key":             "PROJ-123",
		"status":          "Prioritized",
		"previous_status": "Grooming",
		"number":          42,
	}

	cases := []struct {
		name  string
		match string
		want  bool
	}{
		{"empty matches anything of the kind", "{}", true},
		{"blank matches", "", true},
		{"single field", `{"key":"PROJ-123"}`, true},
		{"every field must match", `{"key":"PROJ-123","status":"Prioritized"}`, true},
		{"one wrong field fails the whole match", `{"key":"PROJ-123","status":"Done"}`, false},
		{"absent field fails", `{"assignee":"kartik"}`, false},
		// A recipe writes what it reads in Jira; the payload's casing is
		// whatever the connector emitted, and one should not fight the other.
		{"case-insensitive", `{"status":"prioritized"}`, true},
		// Recipes are YAML, so every match value arrives as a string.
		{"number compared as text", `{"number":"42"}`, true},
		{"unreadable match never fires", `not json`, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := waiterMatches(c.match, payload); got != c.want {
				t.Errorf("waiterMatches(%q) = %v, want %v", c.match, got, c.want)
			}
		})
	}
}

// A run parks on a ticket transition and is revived by the event that matches.
// This is the whole of "wait until somebody prioritises this", so it is worth
// asserting end to end rather than trusting the pieces.
func TestAWaiterIsResolvedByTheEventItMatches(t *testing.T) {
	rt := runtimeWithStore(t)

	arm := func(id, step, kind, match string) {
		t.Helper()
		if err := rt.store.ArmWaiter(store.Waiter{
			ID: id, ExecutionID: "exec-1", Loop: "dev-implement", Step: step,
			EventKind: kind, MatchJSON: match,
		}); err != nil {
			t.Fatal(err)
		}
	}

	arm("w-match", "await-prioritised", "jira.issue.updated", `{"key":"PROJ-1","status":"Prioritized"}`)
	arm("w-other-ticket", "await-other", "jira.issue.updated", `{"key":"PROJ-2","status":"Prioritized"}`)
	arm("w-other-kind", "await-pr", "github.pr.opened", `{"repo":"acme/api"}`)

	rt.resolveWaiters(context.Background(), "jira.issue.updated", map[string]any{
		"key": "PROJ-1", "status": "Prioritized", "summary": "Fix the retry ladder",
	})

	// The matching waiter carries the event through to the resumed run.
	res, ok, err := rt.store.WaiterResult("exec-1", "await-prioritised")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the matching waiter was not resolved")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(res), &got); err != nil {
		t.Fatalf("stored result is not readable: %v", err)
	}
	if got["summary"] != "Fix the retry ladder" {
		t.Errorf("the resumed run gets the event payload, got %v", got)
	}

	// A different ticket and a different event kind are untouched — the bug
	// this guards is one transition waking every parked run.
	for _, step := range []string{"await-other", "await-pr"} {
		if _, ok, err := rt.store.WaiterResult("exec-1", step); err != nil {
			t.Fatal(err)
		} else if ok {
			t.Errorf("%s should still be waiting", step)
		}
	}
}

// A wait that ran out of time still resumes, so the recipe decides what a
// timeout means rather than the run vanishing.
func TestAnExpiredWaiterResolvesWithATimeout(t *testing.T) {
	rt := runtimeWithStore(t)

	past := time.Now().Add(-time.Hour)
	if err := rt.store.ArmWaiter(store.Waiter{
		ID: "w-late", ExecutionID: "exec-2", Loop: "dev-groom", Step: "await-answer",
		EventKind: "jira.comment.created", MatchJSON: "{}", ExpiresAt: &past,
	}); err != nil {
		t.Fatal(err)
	}

	expired, err := rt.store.ExpireWaiters(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 {
		t.Fatalf("expired %d waiters, want 1", len(expired))
	}

	res, ok, err := rt.store.WaiterResult("exec-2", "await-answer")
	if err != nil || !ok {
		t.Fatalf("expired waiter should read as resolved (ok=%v err=%v)", ok, err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(res), &got); err != nil {
		t.Fatal(err)
	}
	if got["timeout"] != true {
		t.Errorf("a timed-out wait must say so, got %v", got)
	}

	// An expired waiter is not also a pending one.
	pending, err := rt.store.PendingWaiters("jira.comment.created")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("expired waiter still pending: %d", len(pending))
	}
}
