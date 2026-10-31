package store

import (
	"testing"
	"time"
)

// A request the turn could not finish becomes a task in one step: the turn's
// row is closed as handed off and the task exists, or neither happened. There
// is no moment in which a restart would find the turn closed and no task, or
// a task and a turn it would run again.
func TestTakingOverARequestClosesItsTurnAndCreatesItsTaskTogether(t *testing.T) {
	s := newTestStore(t)
	if owned, err := s.StartAgentTurn(AgentTurn{ID: "t1", AgentID: "nexus", EventID: "ev-1", EventKind: "comms.message", Attempt: 1}); err != nil || !owned {
		t.Fatalf("start turn: owned=%v err=%v", owned, err)
	}

	task, created, err := s.TakeOverRequest(Task{
		RequestID: "ev-1", AgentID: "nexus", Goal: "Make the quarterly deck for Northwind",
		Context: "the operator's chat, last ten messages", ChannelID: "whatsapp-main", Target: "chat-7@s.whatsapp.net",
	}, "out of time after 19 tool calls")
	if err != nil || !created {
		t.Fatalf("take over: created=%v err=%v", created, err)
	}
	if task.ID != TaskIDForRequest("ev-1") || task.RequestID != "ev-1" {
		t.Errorf("task id %q request %q, want the request's own", task.ID, task.RequestID)
	}
	got, found, err := s.GetTask(task.ID)
	if err != nil || !found {
		t.Fatalf("the task was not stored: %v", err)
	}
	if got.Context != "the operator's chat, last ten messages" || got.Target != "chat-7@s.whatsapp.net" || got.Status != TaskOpen {
		t.Errorf("stored task = %+v", got)
	}
	if st := turnStatusOf(t, s, "ev-1"); st != TurnHandedOff {
		t.Errorf("turn status %q, want %q", st, TurnHandedOff)
	}

	// The turn that was taken over finishing later does not reopen it.
	if err := s.FinishAgentTurn("ev-1", TurnOK, ""); err != nil {
		t.Fatal(err)
	}
	if st := turnStatusOf(t, s, "ev-1"); st != TurnHandedOff {
		t.Errorf("finishing the turn rewrote a hand-off to %q", st)
	}
	// Nor does a redelivery of its event start the turn again.
	if owned, _ := s.StartAgentTurn(AgentTurn{ID: "t2", AgentID: "nexus", EventID: "ev-1", Attempt: 1}); owned {
		t.Error("a redelivered event re-opened a request a task owns")
	}

	// Taking it over twice is the same task, not a second one.
	again, created, err := s.TakeOverRequest(Task{RequestID: "ev-1", Goal: "something else"}, "again")
	if err != nil || created || again.ID != task.ID || again.Goal != task.Goal {
		t.Errorf("second take-over = %+v created=%v err=%v, want the first task", again, created, err)
	}
}

// A request with no journal row of its own (the model created its task while
// the turn was still running) is taken over all the same.
func TestTakingOverARequestWithoutATurnRowStillCreatesTheTask(t *testing.T) {
	s := newTestStore(t)
	task, created, err := s.TakeOverRequest(Task{RequestID: "ask-9", Goal: "Summarise the Lakeside thread"}, "")
	if err != nil || !created {
		t.Fatalf("take over: created=%v err=%v", created, err)
	}
	if _, found, _ := s.GetTaskForRequest("ask-9"); !found {
		t.Errorf("no task for the request after taking it over (%s)", task.ID)
	}
	if _, found, _ := s.GetTaskForRequest("ask-10"); found {
		t.Error("a request nobody took over has a task")
	}
}

// Delivery is recorded, so a crash between sending and recording is found
// on the next attempt rather than sent twice.
func TestADeliveryIsClaimedOnceAndRememberedAsSent(t *testing.T) {
	s := newTestStore(t)
	d := Delivery{Key: "result:task-1", ChannelID: "whatsapp-main", Target: "chat-7", Content: "Here is the deck."}

	state, _, err := s.BeginDelivery(d)
	if err != nil || state != DeliveryNew {
		t.Fatalf("first claim = %q, %v; want new", state, err)
	}
	// An attempt that began and never said whether it sent is pending: the
	// caller must look before sending again.
	state, since, err := s.BeginDelivery(d)
	if err != nil || state != DeliveryPending || since.IsZero() {
		t.Fatalf("second claim = %q (since %v), %v; want pending", state, since, err)
	}
	if err := s.FinishDelivery(d.Key); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := s.BeginDelivery(d); state != DeliverySent {
		t.Fatalf("after finishing, claim = %q; want sent", state)
	}
}

// An earlier attempt that did send is found by what reached the chat.
func TestSentSinceFindsWhatReachedTheChat(t *testing.T) {
	s := newTestStore(t)
	before := time.Now().Add(-time.Minute)
	if err := s.SaveChannelMessage(StoredChannelMessage{ID: "m1", ChannelID: "chat-7", ChannelType: "whatsapp",
		Direction: "outbound", Content: "Here is the deck."}); err != nil {
		t.Fatal(err)
	}
	if !s.SentSince("chat-7", " Here is the deck. ", before) {
		t.Error("an outbound message with the same words was not found")
	}
	if s.SentSince("chat-7", "Something else", before) {
		t.Error("different words counted as sent")
	}
	if s.SentSince("chat-8", "Here is the deck.", before) {
		t.Error("another chat's message counted as sent")
	}
	if s.SentSince("chat-7", "Here is the deck.", time.Now().Add(time.Hour)) {
		t.Error("a message from before the attempt counted as sent")
	}
}

// At start every task marked working belongs to a process that is gone, so
// it is due again now — not half an hour after the round began.
func TestATaskClaimedBeforeAStartIsDueAgainAtOnce(t *testing.T) {
	s := newTestStore(t)
	task, _ := s.CreateTask(Task{Goal: "write the release notes"})
	in30 := time.Now().Add(30 * time.Minute)
	if err := s.UpdateTask(task.ID, TaskUpdate{Status: TaskWorking, NextActionAt: &in30, BumpAttempt: true}); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(time.Second)
	if n, err := s.ReleaseStuckTasks(started); err != nil || n != 1 {
		t.Fatalf("released %d, %v; want the one", n, err)
	}
	due, _ := s.DueTasks(time.Now(), 5)
	if len(due) != 1 || due[0].ID != task.ID {
		t.Fatalf("due = %+v, want the released task", due)
	}
}

func turnStatusOf(t *testing.T, s *Store, eventID string) string {
	t.Helper()
	turns, err := s.RecentTurns("nexus", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range turns {
		if tr.EventID == eventID {
			return tr.Status
		}
	}
	t.Fatalf("no turn for %s", eventID)
	return ""
}
