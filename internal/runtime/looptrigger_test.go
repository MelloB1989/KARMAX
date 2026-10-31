package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// A loop's triggering event is owned from delivery: a crash at any point
// before its run ends — while the workflow triaged it, while it waited for a
// busy loop, before it reached the full agent — replays it on the next start,
// with its message, once.

// recordingLoop is a loop that remembers what each run was triggered by.
type recordingLoop struct {
	mu   sync.Mutex
	runs []loopkit.Trigger
	fail error
}

func (r *recordingLoop) loop(name string) loopkit.Loop {
	return loopkit.Loop{Name: name, Run: func(_ context.Context, k loopkit.Kit) error {
		r.mu.Lock()
		r.runs = append(r.runs, k.Trigger())
		err := r.fail
		r.mu.Unlock()
		return err
	}}
}

func (r *recordingLoop) got() []loopkit.Trigger {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]loopkit.Trigger(nil), r.runs...)
}

func groupMessage(eventID string) loopkit.Trigger {
	return loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: map[string]any{
		"event_id": eventID, "event_kind": "comms.message", "channel_id": "120363000000000001@g.us",
		"content": "@karmax make the Northwind deck", payloadEventAt: time.Now().UTC().Format(time.RFC3339Nano),
	}}
}

// restartWith is a new daemon on the same database, with the loop installed.
func restartWith(t *testing.T, before *KarmaxRuntime, l loopkit.Loop) *KarmaxRuntime {
	t.Helper()
	rt := loopRuntime(t)
	rt.store = before.store
	rt.loopkitLoops = map[string]loopkit.Loop{l.Name: l}
	return rt
}

func triggerStatus(t *testing.T, rt *KarmaxRuntime, loop, eventID string) store.LoopTrigger {
	t.Helper()
	row, found, err := rt.store.GetLoopTrigger(store.LoopTriggerKey(loop, eventID))
	if err != nil || !found {
		t.Fatalf("no trigger row for %s/%s: %v", loop, eventID, err)
	}
	return row
}

// Killed after the event was delivered and before the run did anything — the
// window in which the subscriber had returned and the bus moved on. The next
// start runs the loop with that message, once.
func TestAnEventDeliveredBeforeACrashIsReplayedWithItsMessage(t *testing.T) {
	before := loopRuntime(t)
	if err := before.ownLoopTrigger("chat-monitor", groupMessage("ev-1")); err != nil {
		t.Fatal(err)
	}

	rec := &recordingLoop{}
	rt := restartWith(t, before, rec.loop("chat-monitor"))
	rt.replayLoopTriggers(context.Background())
	waitUntil(t, "the replay", func() bool { return len(rec.got()) == 1 })
	waitUntil(t, "the trigger to close", func() bool {
		return triggerStatus(t, rt, "chat-monitor", "ev-1").Status == store.LoopTriggerDone
	})
	got := rec.got()[0]
	if got.Kind != loopkit.TriggerEvent || got.Payload["content"] != "@karmax make the Northwind deck" || got.Payload["replayed"] != true {
		t.Errorf("replayed as %+v, want the message, marked replayed", got)
	}

	rt.replayLoopTriggers(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := len(rec.got()); n != 1 {
		t.Errorf("a second start replayed it again (%d runs)", n)
	}
}

// The subscriber owns the event before it returns, and a redelivery of the
// same event (the bus is at-least-once) starts no second run.
func TestDeliveringAnEventToALoopOwnsItFirstAndOnlyOnce(t *testing.T) {
	rt := loopRuntime(t)
	rec := &recordingLoop{}
	l := rec.loop("chat-monitor")
	rt.loopkitLoops = map[string]loopkit.Loop{l.Name: l}
	trig := groupMessage("ev-9")

	if err := rt.deliverToLoops(context.Background(), []string{l.Name, "not-installed"}, trig); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := rt.store.GetLoopTrigger(store.LoopTriggerKey(l.Name, "ev-9")); !found {
		t.Fatal("the event was not owned when the subscriber returned")
	}
	waitUntil(t, "the run", func() bool { return len(rec.got()) == 1 })
	if err := rt.deliverToLoops(context.Background(), []string{l.Name}, trig); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(rec.got()); n != 1 {
		t.Errorf("a redelivered event ran the loop %d times", n)
	}
}

// Killed mid-run, while the loop was working on the message: the same.
func TestAnEventWhoseRunWasCutOffIsReplayed(t *testing.T) {
	before := loopRuntime(t)
	trig := groupMessage("ev-2")
	if err := before.ownLoopTrigger("chat-monitor", trig); err != nil {
		t.Fatal(err)
	}
	before.markTrigger("chat-monitor", trig, store.LoopTriggerRunning, true)

	rec := &recordingLoop{}
	rt := restartWith(t, before, rec.loop("chat-monitor"))
	rt.replayLoopTriggers(context.Background())
	waitUntil(t, "the replay", func() bool { return len(rec.got()) == 1 })
	if a := triggerStatus(t, rt, "chat-monitor", "ev-2").Attempts; a < 2 {
		t.Errorf("attempts = %d, want the replay counted", a)
	}
}

// Killed while the message waited for a busy loop: the queue was memory, the
// row was not.
func TestAnEventQueuedBehindABusyLoopSurvivesACrash(t *testing.T) {
	before := loopRuntime(t)
	trig := groupMessage("ev-3")
	if err := before.ownLoopTrigger("chat-monitor", trig); err != nil {
		t.Fatal(err)
	}
	before.markTrigger("chat-monitor", trig, store.LoopTriggerQueued, false)

	rec := &recordingLoop{}
	rt := restartWith(t, before, rec.loop("chat-monitor"))
	rt.replayLoopTriggers(context.Background())
	waitUntil(t, "the replay", func() bool { return len(rec.got()) == 1 })
}

// A run that had already sent something is not replayed — that would say it
// again in other words — and the operator is told instead. One that handed
// its message to a request that still exists is that request's: the request
// is recovered, the message not replayed into a second one.
func TestAReplayNeverRepeatsWhatARunDidOrOwnedElsewhere(t *testing.T) {
	before := loopRuntime(t)
	acted, owned := groupMessage("ev-4"), groupMessage("ev-5")
	for _, tr := range []loopkit.Trigger{acted, owned} {
		if err := before.ownLoopTrigger("chat-monitor", tr); err != nil {
			t.Fatal(err)
		}
	}
	before.markTriggerActed("chat-monitor", acted)
	before.setTriggerOwner("chat-monitor", owned, "ask-77")
	if _, err := before.store.StartAgentTurn(store.AgentTurn{ID: "t", AgentID: "agent-1", EventID: "ask-77",
		EventKind: "request.ask", Attempt: 1}); err != nil {
		t.Fatal(err)
	}

	rec := &recordingLoop{}
	rt := restartWith(t, before, rec.loop("chat-monitor"))
	rt.replayLoopTriggers(context.Background())
	time.Sleep(100 * time.Millisecond)
	if n := len(rec.got()); n != 0 {
		t.Fatalf("replayed %d triggers that must not be", n)
	}
	for _, id := range []string{"ev-4", "ev-5"} {
		if st := triggerStatus(t, rt, "chat-monitor", id).Status; st != store.LoopTriggerDone {
			t.Errorf("%s left %q", id, st)
		}
	}
	notes, _ := rt.store.ListNotifications(10)
	if len(notes) != 1 {
		t.Errorf("the operator got %d notices, want one about the run cut off after acting", len(notes))
	}

	// Owned by a request that was never journalled (the crash came between
	// the two): the message is replayed, or nothing would answer it.
	orphan := groupMessage("ev-6")
	if err := rt.ownLoopTrigger("chat-monitor", orphan); err != nil {
		t.Fatal(err)
	}
	rt.setTriggerOwner("chat-monitor", orphan, "ask-never-journalled")
	rt.replayLoopTriggers(context.Background())
	waitUntil(t, "the orphan's replay", func() bool { return len(rec.got()) == 1 })
}

// A run that fails is retried with its message — not as a bare "retry" with
// none — and a run that succeeds closes the triggers that waited before it in
// the same chat, which it read with the thread.
func TestAFailedRunIsRetriedWithItsMessage(t *testing.T) {
	rt := loopRuntime(t)
	rec := &recordingLoop{fail: errors.New("the model was overloaded")}
	l := rec.loop("chat-monitor")
	rt.loopkitLoops = map[string]loopkit.Loop{l.Name: l}

	trig := groupMessage("ev-7")
	if err := rt.ownLoopTrigger(l.Name, trig); err != nil {
		t.Fatal(err)
	}
	rt.runLoopDurable(context.Background(), l, trig, 1)
	if st := triggerStatus(t, rt, l.Name, "ev-7").Status; st != store.LoopTriggerRetry {
		t.Fatalf("after a failed run the trigger is %q, want retry", st)
	}

	rec.mu.Lock()
	rec.fail = nil
	rec.mu.Unlock()
	if !rt.retryLoopTriggers(context.Background(), l) {
		t.Fatal("the retry found no message to retry")
	}
	waitUntil(t, "the retry", func() bool { return len(rec.got()) == 2 })
	if got := rec.got()[1]; got.Payload["content"] != "@karmax make the Northwind deck" {
		t.Errorf("retried with %+v, want the message", got.Payload)
	}
	waitUntil(t, "the trigger to close", func() bool {
		return triggerStatus(t, rt, l.Name, "ev-7").Status == store.LoopTriggerDone
	})
}

// A message too old to act on by the time the daemon is back is not answered
// now.
func TestAStaleEventIsNotReplayed(t *testing.T) {
	before := loopRuntime(t)
	old := groupMessage("ev-8")
	old.Payload[payloadEventAt] = time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339Nano)
	if err := before.ownLoopTrigger("chat-monitor", old); err != nil {
		t.Fatal(err)
	}
	rec := &recordingLoop{}
	rt := restartWith(t, before, rec.loop("chat-monitor"))
	rt.replayLoopTriggers(context.Background())
	time.Sleep(50 * time.Millisecond)
	if len(rec.got()) != 0 {
		t.Error("a three-hour-old message was answered after a restart")
	}
}
