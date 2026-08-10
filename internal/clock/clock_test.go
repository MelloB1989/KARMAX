package clock

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func newClock(t *testing.T) (*Clock, *store.Store, *bus.Log) {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "k.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	b := bus.NewLog(s, store.DefaultWorkspace, zap.NewNop())
	c := New(s, b, store.DefaultWorkspace, zap.NewNop())
	c.tick = 10 * time.Millisecond
	return c, s, b
}

func consume(t *testing.T, b *bus.Log) <-chan bus.Event {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out := make(chan bus.Event, 8)
	b.Consume(ctx, "test", []bus.EventKind{bus.EventTimerFired}, func(_ context.Context, e bus.Event) error {
		out <- e
		return nil
	})
	return out
}

func TestATimerFires(t *testing.T) {
	c, _, b := newClock(t)
	got := consume(t, b)

	if err := c.After(store.Timer{ID: "t1", Loop: "digest", Payload: map[string]any{"why": "check back"}}, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	select {
	case e := <-got:
		if e.Payload["timer_id"] != "t1" || e.Payload["loop"] != "digest" {
			t.Errorf("payload = %+v", e.Payload)
		}
		if e.Payload["why"] != "check back" {
			t.Error("the caller's payload was not carried through")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the timer never fired")
	}
}

// The point of the whole thing: a deadline that passed while nothing was
// running still fires when something starts again.
func TestATimerSetBeforeADowntimeStillFires(t *testing.T) {
	c, s, b := newClock(t)
	if err := s.SetTimer(store.Timer{
		ID: "overdue", Workspace: store.DefaultWorkspace, Kind: string(bus.EventTimerFired),
		Loop: "digest", FireAt: time.Now().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	got := consume(t, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	select {
	case e := <-got:
		if e.Payload["timer_id"] != "overdue" {
			t.Errorf("fired the wrong timer: %+v", e.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a timer that came due during downtime never fired")
	}
}

func TestATimerFiresOnceEvenIfTheSweepRepeats(t *testing.T) {
	c, s, _ := newClock(t)
	if err := c.After(store.Timer{ID: "once", Loop: "l"}, -time.Second); err != nil {
		t.Fatal(err)
	}

	// Three sweeps. The second and third find nothing, and even if the mark had
	// failed the deterministic event id would keep the append to one.
	c.sweep()
	c.sweep()
	c.sweep()

	events, err := s.LogEventsAfter(store.DefaultWorkspace, 0, []string{string(bus.EventTimerFired)}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("a single timer produced %d events", len(events))
	}
}

// Publishing before marking means a crash in between republishes. That must not
// double-fire, which is what the derived event id buys.
func TestAReFiredTimerStillAppendsOnce(t *testing.T) {
	c, s, _ := newClock(t)
	at := time.Now().Add(-time.Minute)
	tm := store.Timer{ID: "crashy", Workspace: store.DefaultWorkspace,
		Kind: string(bus.EventTimerFired), Loop: "l", FireAt: at}
	if err := s.SetTimer(tm); err != nil {
		t.Fatal(err)
	}

	c.sweep()
	// Simulate the crash: the mark is undone, so the next sweep sees it as due.
	if err := s.SetTimer(tm); err != nil {
		t.Fatal(err)
	}
	c.sweep()

	events, err := s.LogEventsAfter(store.DefaultWorkspace, 0, []string{string(bus.EventTimerFired)}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Errorf("re-firing appended %d events, want 1", len(events))
	}
}

func TestReArmingMovesTheDeadlineRatherThanAddingATimer(t *testing.T) {
	c, _, _ := newClock(t)
	if err := c.After(store.Timer{ID: "x", Loop: "l"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := c.After(store.Timer{ID: "x", Loop: "l"}, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	pending, err := c.Pending(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("re-arming produced %d timers", len(pending))
	}
	if time.Until(pending[0].FireAt) < 90*time.Minute {
		t.Error("the deadline did not move")
	}
}

func TestCancellingDisarms(t *testing.T) {
	c, _, _ := newClock(t)
	if err := c.After(store.Timer{ID: "x", Loop: "l"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel("x"); err != nil {
		t.Fatal(err)
	}
	pending, _ := c.Pending(10)
	if len(pending) != 0 {
		t.Errorf("%d timers survived cancellation", len(pending))
	}
	// Cancelling something that is not there is not an error.
	if err := c.Cancel("never-existed"); err != nil {
		t.Errorf("cancelling an unknown timer failed: %v", err)
	}
}

func TestPruningKeepsArmedTimersHoweverDistant(t *testing.T) {
	c, s, _ := newClock(t)
	if err := c.After(store.Timer{ID: "next-year", Loop: "l"}, 365*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTimer(store.Timer{ID: "old", Workspace: store.DefaultWorkspace,
		Kind: string(bus.EventTimerFired), FireAt: time.Now().Add(-72 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkTimerFired("old", time.Now().Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneTimers(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d timers, want 1", n)
	}
	pending, _ := c.Pending(10)
	if len(pending) != 1 || pending[0].ID != "next-year" {
		t.Error("an armed far-future timer was pruned")
	}
}

func TestLoopTimersAreCancelledTogether(t *testing.T) {
	c, s, _ := newClock(t)
	for _, id := range []string{"a", "b"} {
		if err := c.After(store.Timer{ID: id, Loop: "doomed"}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.After(store.Timer{ID: "c", Loop: "kept"}, time.Hour); err != nil {
		t.Fatal(err)
	}

	n, err := s.CancelLoopTimers("doomed")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("cancelled %d timers, want 2", n)
	}
	pending, _ := c.Pending(10)
	if len(pending) != 1 || pending[0].Loop != "kept" {
		t.Errorf("remaining timers = %+v", pending)
	}
}
