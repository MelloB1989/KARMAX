package store

import (
	"testing"
	"time"
)

// A loop's triggering event is owned from the moment it is delivered: a row
// written before the bus moves past it, followed through the run, and still
// there — with its payload — when the daemon dies before the run ends.
func TestALoopTriggerIsKeptUntilItsRunEnds(t *testing.T) {
	s := newTestStore(t)
	trig := LoopTrigger{Loop: "chat-monitor", EventID: "ev-1", Conversation: "channel_id:group-1",
		Kind: "event", Payload: `{"content":"make the deck"}`}

	created, err := s.RecordLoopTrigger(trig)
	if err != nil || !created {
		t.Fatalf("record: created=%v err=%v", created, err)
	}
	// Delivered twice (the bus is at-least-once): one row.
	if created, _ := s.RecordLoopTrigger(trig); created {
		t.Error("a redelivered event was recorded twice")
	}
	key := LoopTriggerKey("chat-monitor", "ev-1")
	if err := s.SetLoopTriggerStatus(key, LoopTriggerRunning, true); err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenLoopTriggers("")
	if err != nil || len(open) != 1 || open[0].Payload != trig.Payload || open[0].Attempts != 1 || open[0].Status != LoopTriggerRunning {
		t.Fatalf("open = %+v, %v; want the running trigger with its payload", open, err)
	}

	if err := s.MarkLoopTriggerActed(key); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLoopTriggerOwner(key, "ask-1"); err != nil {
		t.Fatal(err)
	}
	got, found, _ := s.GetLoopTrigger(key)
	if !found || !got.Acted || got.Owner != "ask-1" {
		t.Errorf("trigger = %+v, want acted and owned", got)
	}

	if err := s.SetLoopTriggerStatus(key, LoopTriggerDone, false); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenLoopTriggers(""); len(open) != 0 {
		t.Errorf("a finished trigger is still open: %+v", open)
	}
}

// A pass over a conversation reads its whole thread, so it answers what
// arrived before it in the same chat: those triggers are done with it.
// Other chats, and later messages, are not.
func TestAPassSupersedesEarlierTriggersInItsConversation(t *testing.T) {
	s := newTestStore(t)
	for _, tr := range []LoopTrigger{
		{Loop: "chat-monitor", EventID: "a", Conversation: "channel_id:group-1"},
		{Loop: "chat-monitor", EventID: "b", Conversation: "channel_id:group-1"},
		{Loop: "chat-monitor", EventID: "c", Conversation: "channel_id:group-2"},
		{Loop: "other-loop", EventID: "a", Conversation: "channel_id:group-1"},
	} {
		if _, err := s.RecordLoopTrigger(tr); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := s.RecordLoopTrigger(LoopTrigger{Loop: "chat-monitor", EventID: "d", Conversation: "channel_id:group-1"}); err != nil {
		t.Fatal(err)
	}
	b, _, _ := s.GetLoopTrigger(LoopTriggerKey("chat-monitor", "b"))
	if _, err := s.SupersedeLoopTriggers("chat-monitor", "channel_id:group-1", b.CreatedAt); err != nil {
		t.Fatal(err)
	}
	open, _ := s.OpenLoopTriggers("")
	left := map[string]bool{}
	for _, o := range open {
		left[o.Loop+"/"+o.EventID] = true
	}
	for _, want := range []string{"chat-monitor/c", "chat-monitor/d", "other-loop/a"} {
		if !left[want] {
			t.Errorf("%s was closed with another chat's or a later message's pass", want)
		}
	}
	if left["chat-monitor/a"] || left["chat-monitor/b"] {
		t.Errorf("earlier triggers in the pass's chat are still open: %v", left)
	}
}
