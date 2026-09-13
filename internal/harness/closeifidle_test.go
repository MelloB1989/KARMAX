package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// writeFakeClaude writes a script that behaves like the real CLI closely
// enough to drive the supervisor's spawn/Send path: it reads one stdin line
// per turn, waits delay, then answers with a single "result" event. It exits
// cleanly on SIGINT/SIGTERM (bash's default disposition, since nothing here
// traps them), so Close() never needs its 3s SIGKILL fallback and these
// tests stay fast.
func writeFakeClaude(t *testing.T, delay time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeclaude.sh")
	body := "#!/bin/bash\n" +
		"while IFS= read -r line; do\n" +
		fmt.Sprintf("  sleep %g\n", delay.Seconds()) +
		"  echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"ok\",\"total_cost_usd\":0.001,\"num_turns\":1,\"duration_ms\":10,\"usage\":{}}'\n" +
		"done\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// newRaceSupervisor is a Supervisor over a fake CLI that answers after delay,
// wide enough that a turn is reliably still in flight when the test wants to
// race a close against it.
func newRaceSupervisor(t *testing.T, delay time.Duration) (*Supervisor, *memStore) {
	t.Helper()
	st := newMemStore()
	sup := New(Config{
		Binary: writeFakeClaude(t, delay), WorkdirRoot: t.TempDir(), MaxLive: 8,
		Policies: map[string]Policy{"chat": {Idle: time.Minute, TurnTimeout: 5 * time.Second}},
		Env:      os.Environ(),
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	return sup, st
}

// This is the exact gap the Task 3 review flagged: open()'s fast-reuse path
// releases the supervisor's lock and hands back a live session BEFORE the
// caller ever calls Send — the call that used to be the only place busy
// became true. Between those two moments the session sat in s.live looking
// idle to anything checking Busy(), even though a turn was already
// committed to running on it.
//
// This is a deterministic reproduction, not a scheduling race: it calls
// open() directly and checks Busy() immediately after, with no Send() call
// in between at all — so it needs no luck to hit the window, and it proves
// the window exists (or doesn't) on every run.
func TestOpenClaimsTheSessionBusyBeforeHandingBackAReuse(t *testing.T) {
	sup, _ := newRaceSupervisor(t, 10*time.Millisecond)
	ctx := context.Background()

	if _, err := sup.Send(ctx, "k", "chat", "warm up"); err != nil {
		t.Fatalf("warm-up turn: %v", err)
	}
	if sup.Busy("k") {
		t.Fatal("precondition: session must be idle after its first turn finishes")
	}

	sess, err := sup.open(ctx, "k", "chat", sup.policy("chat"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !sess.Busy() {
		t.Fatal("open() handed back a session for reuse without claiming it busy first — " +
			"a recycler's Busy() check made in this window would see it as idle and close it " +
			"out from under the Send() this caller is about to make")
	}
}

// With the claim in place, CloseIfIdle has nothing to race: the session is
// already busy by the time anything else could observe it, so a close
// attempted in the same window this test used to prove was unguarded must
// now be refused.
func TestCloseIfIdleWontCloseASessionOpenJustClaimed(t *testing.T) {
	sup, _ := newRaceSupervisor(t, 10*time.Millisecond)
	ctx := context.Background()

	if _, err := sup.Send(ctx, "k", "chat", "warm up"); err != nil {
		t.Fatalf("warm-up turn: %v", err)
	}

	sess, err := sup.open(ctx, "k", "chat", sup.policy("chat"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if sup.CloseIfIdle("k") {
		t.Fatal("CloseIfIdle closed a session open() had just claimed for an upcoming Send")
	}
	if !sess.Alive() {
		t.Fatal("the session's process was torn down even though CloseIfIdle reported it did not close")
	}

	// Finish the claim's lifecycle cleanly rather than leaving it dangling
	// for Shutdown to force through.
	if _, err := sess.Send(ctx, "second", 5*time.Second, nil); err != nil {
		t.Fatalf("second turn: %v", err)
	}
}

// The end-to-end version of the same guarantee, isolating the actual race
// rather than hoping to schedule into it: open() runs synchronously first
// (the same call SendWith makes), so every iteration starts from exactly the
// dangerous state — a reused session handed back, no Send() yet — and only
// then races a real Send() against a real CloseIfIdle on two goroutines.
//
// Under the pre-fix stub this fails almost every iteration, since
// CloseIfIdle is a couple of uncontended map/atomic ops and Send() has to
// spin up a goroutine and take the session's own mutex first — CloseIfIdle
// usually wins the race outright. Under the fix it cannot fail at all: open()
// has already set busy under the supervisor's lock before either goroutine
// starts, so CloseIfIdle sees a busy session on every single iteration.
func TestCloseIfIdleNeverKillsATurnItRacesAgainstConcurrently(t *testing.T) {
	sup, _ := newRaceSupervisor(t, 40*time.Millisecond)
	ctx := context.Background()

	if _, err := sup.Send(ctx, "race", "chat", "warm up"); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	const iterations = 30
	for i := 0; i < iterations; i++ {
		sess, err := sup.open(ctx, "race", "chat", sup.policy("chat"), Options{})
		if err != nil {
			t.Fatalf("iteration %d: open: %v", i, err)
		}

		var wg sync.WaitGroup
		var sendErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, sendErr = sess.Send(ctx, "concurrent turn", 5*time.Second, nil)
		}()
		go func() {
			defer wg.Done()
			sup.CloseIfIdle("race")
		}()
		wg.Wait()

		if sendErr != nil {
			t.Fatalf("iteration %d: a concurrent recycle killed an in-flight turn: %v", i, sendErr)
		}
	}
}
