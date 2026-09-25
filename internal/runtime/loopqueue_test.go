package runtime

import (
	"testing"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

func event(chat, body string) loopkit.Trigger {
	return loopkit.Trigger{
		Kind:    loopkit.TriggerEvent,
		Payload: map[string]any{"channel_id": chat, "content": body},
	}
}

// The failure this queue was written for: a message the operator had tagged
// the assistant in arrived while a pass was running and was thrown away.
func TestAQueuedEventIsNotLost(t *testing.T) {
	p := newPendingTriggers()
	if !p.add("wa-monitor", event("group@g.us", "@karmax find my cab costs")) {
		t.Fatal("the event was refused")
	}
	got := p.take("wa-monitor")
	if len(got) != 1 {
		t.Fatalf("took %d events, want 1", len(got))
	}
	if got[0].Payload["content"] != "@karmax find my cab costs" {
		t.Errorf("the wrong event came back: %v", got[0].Payload)
	}
}

// Two messages in one chat must not become two replies — that is what the
// lease was protecting, and the queue has to keep protecting it.
func TestSameChatCoalescesToTheLatest(t *testing.T) {
	p := newPendingTriggers()
	p.add("wa-monitor", event("group@g.us", "first"))
	p.add("wa-monitor", event("group@g.us", "second"))
	p.add("wa-monitor", event("group@g.us", "third"))

	got := p.take("wa-monitor")
	if len(got) != 1 {
		t.Fatalf("took %d events, want them coalesced into 1", len(got))
	}
	if got[0].Payload["content"] != "third" {
		t.Errorf("content = %v, want the latest message", got[0].Payload["content"])
	}
}

// Different chats have no ordering relationship and must all survive.
func TestDifferentChatsAllSurvive(t *testing.T) {
	p := newPendingTriggers()
	p.add("wa-monitor", event("a@g.us", "one"))
	p.add("wa-monitor", event("b@s.whatsapp.net", "two"))
	p.add("wa-monitor", event("c@g.us", "three"))

	got := p.take("wa-monitor")
	if len(got) != 3 {
		t.Fatalf("took %d events, want 3", len(got))
	}
	// Oldest conversation first, so a busy chat cannot starve a quiet one.
	if got[0].Payload["channel_id"] != "a@g.us" {
		t.Errorf("first out was %v, want the longest-waiting chat", got[0].Payload["channel_id"])
	}
}

// A later message in an already-queued chat must not re-order that chat behind
// chats queued after it.
func TestCoalescingKeepsTheOriginalPlace(t *testing.T) {
	p := newPendingTriggers()
	p.add("wa-monitor", event("a@g.us", "one"))
	p.add("wa-monitor", event("b@g.us", "two"))
	p.add("wa-monitor", event("a@g.us", "one-again"))

	got := p.take("wa-monitor")
	if len(got) != 2 {
		t.Fatalf("took %d events, want 2", len(got))
	}
	if got[0].Payload["channel_id"] != "a@g.us" || got[0].Payload["content"] != "one-again" {
		t.Errorf("first out = %v, want a@g.us carrying the newer message", got[0].Payload)
	}
}

func TestTakeEmptiesTheQueue(t *testing.T) {
	p := newPendingTriggers()
	p.add("wa-monitor", event("a@g.us", "one"))
	p.take("wa-monitor")
	if got := p.take("wa-monitor"); len(got) != 0 {
		t.Errorf("took %d events from a drained queue", len(got))
	}
	if p.waiting("wa-monitor") != 0 {
		t.Error("the queue should report empty")
	}
}

// Unbounded growth here would be a way to run the daemon out of memory.
func TestQueueIsBounded(t *testing.T) {
	p := newPendingTriggers()
	for i := 0; i < maxPendingPerLoop; i++ {
		if !p.add("wa-monitor", event(string(rune('a'+i%26))+string(rune('a'+i/26))+"@g.us", "x")) {
			t.Fatalf("event %d was refused below the limit", i)
		}
	}
	if p.add("wa-monitor", event("overflow@g.us", "x")) {
		t.Error("the queue accepted an event past its limit")
	}
	// A chat already queued is still accepted at the limit, because it
	// replaces rather than grows.
	if !p.add("wa-monitor", event("aa@g.us", "replacement")) {
		t.Error("an already-queued chat should still coalesce at the limit")
	}
}

func TestLoopsDoNotShareAQueue(t *testing.T) {
	p := newPendingTriggers()
	p.add("wa-monitor", event("a@g.us", "one"))
	p.add("gchat-watch", event("a@g.us", "two"))

	if got := p.take("wa-monitor"); len(got) != 1 || got[0].Payload["content"] != "one" {
		t.Errorf("wa-monitor got %v", got)
	}
	if got := p.take("gchat-watch"); len(got) != 1 || got[0].Payload["content"] != "two" {
		t.Errorf("gchat-watch got %v", got)
	}
}

func TestConversationKeys(t *testing.T) {
	for name, tc := range map[string]struct {
		trigger loopkit.Trigger
		want    string
	}{
		"channel_id": {loopkit.Trigger{Payload: map[string]any{"channel_id": "x@g.us"}}, "channel_id:x@g.us"},
		"chat_id":    {loopkit.Trigger{Payload: map[string]any{"chat_id": "y"}}, "chat_id:y"},
		"unkeyed":    {loopkit.Trigger{Kind: "event"}, "kind:event"},
		"empty id":   {loopkit.Trigger{Kind: "event", Payload: map[string]any{"channel_id": ""}}, "kind:event"},
	} {
		if got := conversationOf(tc.trigger); got != tc.want {
			t.Errorf("%s: key = %q, want %q", name, got, tc.want)
		}
	}
}
