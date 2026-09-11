package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// The wire carries two events the harness never emits, and they bracket the
// ones it does.
func TestStreamBracketsHarnessEventsWithConversationAndDone(t *testing.T) {
	var lines []string
	w := &lineSink{onLine: func(s string) { lines = append(lines, s) }}

	streamTurn(w, "sess-1", true, func(sink func(harnessEvent)) (string, error) {
		sink(harnessEvent{Kind: "text", Text: "Found "})
		sink(harnessEvent{Kind: "tool", Tool: "Bash", Phase: "start"})
		sink(harnessEvent{Kind: "text", Text: "14."})
		return "Found 14.", nil
	})

	kinds := kindsOf(t, lines)
	want := []string{"conversation", "text", "tool", "text", "done"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
}

// An existing conversation is not re-announced; the client already has the id.
func TestExistingConversationIsNotAnnounced(t *testing.T) {
	var lines []string
	w := &lineSink{onLine: func(s string) { lines = append(lines, s) }}
	streamTurn(w, "sess-1", false, func(sink func(harnessEvent)) (string, error) {
		sink(harnessEvent{Kind: "text", Text: "hi"})
		return "hi", nil
	})
	if got := kindsOf(t, lines); got[0] != "text" {
		t.Fatalf("first event = %q, want text", got[0])
	}
}

// A failure after partial output still ends the stream with something the
// client can act on. Headers left long ago; a status code is not available.
func TestFailureEndsTheStream(t *testing.T) {
	var lines []string
	w := &lineSink{onLine: func(s string) { lines = append(lines, s) }}
	streamTurn(w, "sess-1", false, func(sink func(harnessEvent)) (string, error) {
		sink(harnessEvent{Kind: "text", Text: "partial"})
		return "", errTest
	})
	kinds := kindsOf(t, lines)
	if kinds[len(kinds)-1] != "error" {
		t.Fatalf("last event = %q, want error", kinds[len(kinds)-1])
	}
}

// A background delegation becomes a ticket event, before done.
func TestBackgroundWorkBecomesATicket(t *testing.T) {
	var lines []string
	w := &lineSink{onLine: func(s string) { lines = append(lines, s) }}
	streamTurn(w, "s", false, func(sink func(harnessEvent)) (string, error) {
		sink(harnessEvent{Kind: "text", Text: "That will take a while."})
		sink(harnessEvent{Kind: "ticket", JobID: "job-1", Text: "Refactor the auth module"})
		return "That will take a while.", nil
	})
	kinds := kindsOf(t, lines)
	if strings.Join(kinds, ",") != "text,ticket,done" {
		t.Fatalf("kinds = %v", kinds)
	}
}

func kindsOf(t *testing.T, lines []string) []string {
	t.Helper()
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var e struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("line is not JSON: %q", l)
		}
		out = append(out, e.Kind)
	}
	return out
}
