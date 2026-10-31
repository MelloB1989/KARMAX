package runtime

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// Crash points.
//
// From the moment a request is accepted it is owned until it is done and
// delivered — across a crash, a restart or a power cut at any point. Each
// test below leaves the database exactly as a daemon killed at one point
// would (written through the same store calls the daemon makes), starts a
// fresh daemon on it, and checks the request is finished once and its
// answer delivered once: never lost, never twice.

// beforeCrash opens the database a killed daemon left behind.
func beforeCrash(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "karmax.db")
	db, err := store.New(path, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	return db, path
}

// restart starts a daemon on what the crash left: the start-up recovery the
// daemon runs, then its task runner.
func restart(t *testing.T, path string) *longWorkFlow {
	t.Helper()
	f := newLongWorkFlow(t, path)
	f.rt.resumeInterruptedTurns()
	return f
}

// askRequestRow is a workflow's ask, journalled the way the agent does
// before the turn starts.
func askRequestRow(t *testing.T, db *store.Store) *agent.Request {
	t.Helper()
	req := agent.NewRequest("ask-crash-1", agent.RequestAsk)
	req.Text = "The operator directly instructed you in the group: make the Northwind onboarding deck."
	req.Asked = "@karmax make the Northwind onboarding deck"
	req.ChannelID, req.Target, req.Where, req.AgentID = waID, teamChat, "the operator, in the chat \"Lakeside team\"", "nexus"
	req.Started = time.Now().Add(-2 * time.Minute)
	if _, err := db.StartAgentTurn(store.AgentTurn{ID: "turn-" + req.ID, AgentID: "nexus", EventID: req.ID,
		EventKind: req.Kind, EventJSON: req.JSON(), Attempt: 1, StartedAt: req.Started}); err != nil {
		t.Fatal(err)
	}
	return req
}

// exactlyOnce checks the chat got the acknowledgement (when wanted), the
// result and the file — each once — and nothing more.
func exactlyOnce(t *testing.T, f *longWorkFlow, target string, wantAck bool) {
	t.Helper()
	n := 2
	if wantAck {
		n = 3
	}
	got := f.waitFor(t, target, n)
	time.Sleep(400 * time.Millisecond)
	got = f.sentTo(target)
	var acks, results, files int
	for _, s := range got {
		switch {
		case s.file == f.deck:
			files++
		case strings.Contains(s.text, "On it"):
			acks++
		case strings.Contains(s.text, "onboarding deck for Northwind"):
			results++
		}
	}
	wantAcks := 0
	if wantAck {
		wantAcks = 1
	}
	if acks != wantAcks || results != 1 || files != 1 || len(got) != n {
		t.Errorf("the chat got %d acks, %d results, %d files (%d messages); want %d, 1, 1:\n%+v",
			acks, results, files, len(got), wantAcks, got)
	}
}

// Killed after the request was journalled, before the turn did anything.
// Its caller (the workflow's run) died with the daemon, so it continues as a
// task: acknowledged, worked, answered — once.
func TestCrashBetweenAcceptingARequestAndStartingIt(t *testing.T) {
	db, path := beforeCrash(t)
	req := askRequestRow(t, db)
	db.Close()

	f := restart(t, path)
	exactlyOnce(t, f, teamChat, true)
	task, found, _ := f.db.GetTaskForRequest(req.ID)
	if !found || task.Status != store.TaskDone {
		t.Fatalf("the request's task = %+v (found %v), want done", task, found)
	}
	if st := turnStatusFor(t, f.db, req.ID); st != store.TurnHandedOff {
		t.Errorf("the request's row says %q, want handed_off", st)
	}

	// A second start finds nothing left to do.
	f.cancel()
	g := restart(t, path)
	time.Sleep(500 * time.Millisecond)
	if again := g.sentTo(teamChat); len(again) != 0 {
		t.Errorf("a second restart delivered again: %+v", again)
	}
}

// Killed while an operator's message was mid-turn. The turn's row is still
// running, so the message is handed back to the agent: its turn runs again,
// outruns its time again, and continues as a task — answered once.
func TestCrashWhileAnOperatorsTurnWasWorking(t *testing.T) {
	db, path := beforeCrash(t)
	evt := operatorMessage("make an onboarding deck for Northwind from the work we did, no filler")
	evt.Timestamp = time.Now()
	raw, _ := json.Marshal(evt)
	if _, err := db.StartAgentTurn(store.AgentTurn{ID: "t-1", AgentID: "nexus", EventID: evt.ID,
		EventKind: string(bus.EventCommsMessage), EventJSON: string(raw), Attempt: 1,
		StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	f := restart(t, path)
	exactlyOnce(t, f, opChat, true)
	if task, found, _ := f.db.GetTaskForRequest(evt.ID); !found || task.Status != store.TaskDone {
		t.Fatalf("task = %+v found=%v", task, found)
	}
}

// Killed mid-task: the request had become a task and been acknowledged, and
// the dead daemon had claimed a round. The task is due again at once — not
// half an hour later — and the acknowledgement is not repeated.
func TestCrashMidTask(t *testing.T) {
	db, path := beforeCrash(t)
	req := askRequestRow(t, db)
	task, _, err := db.TakeOverRequest(store.Task{RequestID: req.ID, AgentID: "nexus", Goal: requestGoal(req),
		Title: requestTitle(req), ChannelID: waID, Target: teamChat}, "out of time")
	if err != nil {
		t.Fatal(err)
	}
	ack := store.Delivery{Key: "ack:" + req.ID, ChannelID: waID, Target: teamChat, Content: handoffAck(req)}
	if _, _, err := db.BeginDelivery(ack); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishDelivery(ack.Key); err != nil {
		t.Fatal(err)
	}
	claim := time.Now().Add(taskStuckAfter)
	if err := db.UpdateTask(task.ID, store.TaskUpdate{Status: store.TaskWorking, NextActionAt: &claim, BumpAttempt: true}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	f := restart(t, path)
	exactlyOnce(t, f, teamChat, false)
	if got, _, _ := f.db.GetTask(task.ID); got.Status != store.TaskDone {
		t.Errorf("task status %q, want done", got.Status)
	}
}

// doneRound records a request task's finished round the way the runner does,
// owing its result and file — and stops there, as a daemon killed right
// after it would.
func doneRound(t *testing.T, db *store.Store, deck string) store.Task {
	t.Helper()
	req := askRequestRow(t, db)
	task, _, err := db.TakeOverRequest(store.Task{RequestID: req.ID, AgentID: "nexus", Goal: "make the deck",
		ChannelID: waID, Target: teamChat}, "out of time")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteTaskRound(task.ID, store.TaskUpdate{Status: store.TaskDone, Progress: "built it"}, []store.Delivery{
		{Key: "task:" + task.ID + ":1:00", ChannelID: waID, Target: teamChat, Content: "Here is the onboarding deck for Northwind: six pages, no filler."},
		{Key: "task:" + task.ID + ":1:01", ChannelID: waID, Target: teamChat, Content: "[file] " + filepath.Base(deck), FilePath: deck},
	}); err != nil {
		t.Fatal(err)
	}
	return task
}

// Killed after the result was recorded and before it was sent. It is owed,
// so the next start sends it — once — without running the work again.
func TestCrashBetweenTheResultAndItsDelivery(t *testing.T) {
	db, path := beforeCrash(t)
	f0 := &longWorkFlow{deck: filepath.Join(filepath.Dir(path), "northwind-deck.pdf")}
	doneRound(t, db, f0.deck)
	db.Close()

	f := newLongWorkFlowDeck(t, path, f0.deck)
	f.rt.resumeInterruptedTurns()
	exactlyOnce(t, f, teamChat, false)
	if owed, _ := f.db.OwedDeliveries("task:"); len(owed) != 0 {
		t.Errorf("still owed after the restart delivered: %+v", owed)
	}
}

// Killed after the result was sent and before that was recorded. The chat
// shows it went out, so the next start records it and sends nothing.
func TestCrashBetweenDeliveryAndItsRecord(t *testing.T) {
	db, path := beforeCrash(t)
	deck := filepath.Join(filepath.Dir(path), "northwind-deck.pdf")
	doneRound(t, db, deck)
	// What comms stores right after each send succeeds.
	for i, content := range []string{"Here is the onboarding deck for Northwind: six pages, no filler.", "[file] " + filepath.Base(deck)} {
		if err := db.SaveChannelMessage(store.StoredChannelMessage{ID: "sent-" + string(rune('a'+i)), ChannelID: teamChat,
			ChannelType: "whatsapp", Direction: "outbound", Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	f := newLongWorkFlowDeck(t, path, deck)
	f.rt.resumeInterruptedTurns()
	time.Sleep(700 * time.Millisecond)
	if again := f.sentTo(teamChat); len(again) != 0 {
		t.Errorf("a result that had gone out was sent again: %+v", again)
	}
	if owed, _ := f.db.OwedDeliveries("task:"); len(owed) != 0 {
		t.Errorf("a delivered result is still owed: %+v", owed)
	}
}

// A request task whose rounds keep failing tells its requester once — what
// failed and that it is still being retried — not every round, and not
// never.
func TestAStalledRequestTaskSaysSoOnce(t *testing.T) {
	f := newLongWorkFlow(t, "")
	task := store.Task{ID: "req-ask-s", RequestID: "ask-s", Title: "Northwind onboarding deck", ChannelID: waID, Target: teamChat}
	cause := errFor("harness unavailable: the account refused: rejected")
	f.rt.tellStalled(task, cause)
	f.rt.tellStalled(task, cause)
	got := f.sentTo(teamChat)
	if len(got) != 1 || !strings.Contains(got[0].text, "retrying") || !strings.Contains(got[0].text, "rejected") {
		t.Errorf("the requester got %+v, want one honest notice", got)
	}
}

type errFor string

func (e errFor) Error() string { return string(e) }

// Killed after the request had been handed over and acknowledged, but before
// that acknowledgement was recorded: the next start sees the chat has it and
// does not acknowledge twice.
func TestCrashBetweenTheAcknowledgementAndItsRecord(t *testing.T) {
	db, path := beforeCrash(t)
	req := askRequestRow(t, db)
	ack := store.Delivery{Key: "ack:" + req.ID, ChannelID: waID, Target: teamChat, Content: handoffAck(req)}
	if _, _, err := db.BeginDelivery(ack); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveChannelMessage(store.StoredChannelMessage{ID: "ack-1", ChannelID: teamChat, ChannelType: "whatsapp",
		Direction: "outbound", Content: ack.Content, SenderID: "status"}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	f := restart(t, path)
	exactlyOnce(t, f, teamChat, false)
}
