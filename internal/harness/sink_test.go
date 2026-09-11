package harness

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The sink sees the turn as it happens, in order: text from the streamed
// deltas, tool calls from the assistant block that carries them.
//
// Without this the desktop chat is a three-minute spinner: the events are
// already parsed on the way to building a Turn and were simply discarded.
func TestSinkSeesTextAndTools(t *testing.T) {
	var got []Event
	sink := func(e Event) { got = append(got, e) }

	replay(sink, []event{
		streamDeltaEvent("Look"),
		assistantEvent(contentBlock{Type: "tool_use", Name: "Bash"}),
		streamDeltaEvent("ing…"),
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

// This is the regression test for the double-delivery trap: the CLI streams
// text as stream_event deltas, then repeats the same text whole in a normal
// assistant event. The fixture is real --include-partial-messages output, so
// this proves emit against the actual wire shape, not a guess at it.
func TestSinkDeliversStreamedTextExactlyOnce(t *testing.T) {
	f, err := os.Open("testdata/partial-messages.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var got []Event
	sink := func(e Event) { got = append(got, e) }

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		emit(sink, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	for _, e := range got {
		if e.Kind == "text" {
			sb.WriteString(e.Text)
		}
	}
	if want := "hello world"; sb.String() != want {
		t.Errorf("assembled text = %q, want %q exactly once", sb.String(), want)
	}
}

// assistantEvent builds an assistant event from content blocks, the shape the
// CLI itself emits, so the sink can be tested without a subprocess.
func assistantEvent(blocks ...contentBlock) event {
	e := event{Type: "assistant"}
	e.Message.Content = blocks
	return e
}

// streamDeltaEvent builds a stream_event carrying one text_delta chunk, the
// shape --include-partial-messages emits per token.
func streamDeltaEvent(text string) event {
	e := event{Type: "stream_event"}
	e.StreamEvent.Type = "content_block_delta"
	e.StreamEvent.Delta.Type = "text_delta"
	e.StreamEvent.Delta.Text = text
	return e
}
