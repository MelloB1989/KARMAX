package agent

import (
	"strings"
	"testing"
)

// The actions that actually repeated, rendered the way the model will read
// them. What matters is that a send is recognisable by recipient and content —
// that is what lets the model tell "I already did this" from "they want it
// again" without any rule about repetition.
func TestRecentActionsDescribeWhatWasDone(t *testing.T) {
	got := describeAction("comms.send", map[string]any{
		"target":  "150285251002514@lid",
		"content": "Hey Shiva, when is Aymaan starting?",
	})
	if !strings.Contains(got, "150285251002514") || !strings.Contains(got, "Aymaan") {
		t.Errorf("a send must name the recipient and what was said, got %q", got)
	}
	if strings.Contains(got, "@lid") {
		t.Errorf("the raw JID suffix is noise in the prompt, got %q", got)
	}

	// Tools whose repetition nobody sees are not worth prompt space.
	if d := describeAction("memory.lookup", map[string]any{"query": "shiva"}); d != "" {
		t.Errorf("an internal read should not be reported as an action, got %q", d)
	}
	// A send with nothing identifiable is not worth a line either.
	if d := describeAction("comms.send", map[string]any{}); d != "" {
		t.Errorf("an empty send should render nothing, got %q", d)
	}
	if d := describeAction("call.start", map[string]any{"to": "917671837092@s.whatsapp.net"}); !strings.HasPrefix(d, "called 917671837092") {
		t.Errorf("a call must be reported, got %q", d)
	}
}

func TestLongMessagesAreTruncatedNotDropped(t *testing.T) {
	long := strings.Repeat("word ", 60)
	got := describeAction("comms.send", map[string]any{"target": "x@lid", "content": long})
	if got == "" {
		t.Fatal("a long message must still be reported")
	}
	if len([]rune(got)) > recentActionChars+60 {
		t.Errorf("a long message must be truncated, got %d chars", len([]rune(got)))
	}
	if !strings.Contains(got, "…") {
		t.Error("truncation should be visible")
	}
}

// The real payloads that produced the repeats, straight from the event log.
// If these do not render, the context is inert and the model learns nothing.
func TestRealSendPayloadsRender(t *testing.T) {
	cases := []map[string]any{
		{"channel_id": "whatsapp-main", "content": "Hey Shiva, quick check — when is Aymaan starting?", "target": "150285251002514@lid"},
		{"channel_id": "whatsapp-main", "content": "Done — messaged Shiva asking when Aymaan is starting.", "target": "5794649083972@lid"},
	}
	for i, input := range cases {
		got := describeAction("comms_send", input)
		if got == "" {
			t.Fatalf("case %d rendered nothing from a real payload: %v", i, input)
		}
		if !strings.Contains(got, "Aymaan") {
			t.Errorf("case %d must carry what was said, got %q", i, got)
		}
	}
	// The dotted spelling reaches the log too and must render identically.
	if describeAction("comms.send", cases[0]) != describeAction("comms_send", cases[0]) {
		t.Error("both spellings of the tool name must render the same action")
	}
}
