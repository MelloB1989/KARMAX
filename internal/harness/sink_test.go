package harness

import "testing"

// The sink sees the turn as it happens, in order.
//
// Without this the desktop chat is a three-minute spinner: the events are
// already parsed on the way to building a Turn and were simply discarded.
func TestSinkSeesTextAndTools(t *testing.T) {
	var got []Event
	sink := func(e Event) { got = append(got, e) }

	replay(sink, []event{
		assistantEvent(contentBlock{Type: "text", Text: "Look"}),
		assistantEvent(contentBlock{Type: "tool_use", Name: "Bash"}),
		assistantEvent(contentBlock{Type: "text", Text: "ing…"}),
	})

	want := []Event{
		{Kind: "text", Text: "Look"},
		{Kind: "tool", Tool: "Bash", Phase: "start"},
		{Kind: "text", Text: "ing…"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A nil sink must change nothing at all. Every existing caller — the task
// runner, loops, harnessSendTool — goes through this same path.
func TestNilSinkIsSafe(t *testing.T) {
	replay(nil, []event{assistantEvent(contentBlock{Type: "text", Text: "hi"})})
}

// assistantEvent builds an assistant event from content blocks, the shape the
// CLI itself emits, so the sink can be tested without a subprocess.
func assistantEvent(blocks ...contentBlock) event {
	e := event{Type: "assistant"}
	e.Message.Content = blocks
	return e
}
