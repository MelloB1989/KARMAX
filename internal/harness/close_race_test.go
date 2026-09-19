package harness

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// --- Step 1, repro A: Supervisor.Close substituted for CloseIfIdle ---------
//
// This is the reviewer's own reproduction from the Task 8 review: take
// TestCloseIfIdleNeverKillsATurnItRacesAgainstConcurrently and swap the call
// under test for the unconditional Close. Unlike CloseIfIdle, Close never
// checks Busy at all, so every iteration tears the session down regardless —
// the "kill" is not a race, it is guaranteed by design. What IS a race is
// what Close does while tearing it down: Session.Close's stdin.Flush (never
// under Session.mu) against Send's own stdin.Write/Flush (under Session.mu,
// but Close doesn't take it) on the very same *bufio.Writer. That is what
// -race is here to catch, independent of whether the kill itself is a bug.
//
// This test's own "kill" count stays 30/30 forever: Step 4 deliberately
// keeps Supervisor.Close unconditional, since two of its four callers are an
// operator explicitly asking to interrupt. What Step 2 actually fixes —
// confirmed by running this under -race before and after — is that doing so
// no longer corrupts the writer while it does it.
func TestSupervisorCloseRacesSendOnTheSharedWriter(t *testing.T) {
	sup, _ := newRaceSupervisor(t, 40*time.Millisecond)
	ctx := context.Background()

	if _, err := sup.Send(ctx, "race", "chat", "warm up"); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	const iterations = 30
	killed := 0
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
			sup.Close("race")
		}()
		wg.Wait()

		if sendErr != nil {
			killed++
			t.Logf("iteration %d: Close interrupted the in-flight turn: %v", i, sendErr)
			// Close tore the session down entirely (unlike CloseIfIdle, which
			// mostly leaves it alone); re-warm it so the next iteration starts
			// from the same state.
			if _, err := sup.Send(ctx, "race", "chat", "warm up again"); err != nil {
				t.Fatalf("iteration %d: re-warm after kill: %v", i, err)
			}
		}
	}
	t.Logf("%d/%d iterations: an unconditional Close interrupted an in-flight turn "+
		"(expected — Close does not check Busy; see Step 4's call-site decisions). "+
		"Run this test under -race: a WARNING: DATA RACE on Session's bufio.Writer "+
		"would be the actual bug (F1's root cause), not the interruption itself.", killed, iterations)
}

// --- Step 1, repro B (as fixed): evictIfFull never kills a session that ----
// --- goes busy in the same window it is being considered for eviction  ----
//
// The reviewer's original reproduction paused evictIfFull's own goroutine,
// with a fake Logger, in the gap between its then-bare s.Busy(key) check and
// its unconditional s.Close(key) call — proving 30/30 that a session which
// went busy in that gap got closed anyway, with a genuine WARNING: DATA RACE
// underneath it (on the same bufio.Writer as repro A). Fixing evictIfFull to
// close its victim through CloseIfIdle (Step 3) closed that exact gap: the
// busy check and the close-and-remove now happen in the same critical
// section, so there is nothing left for a fake Logger to pause it inside of
// — the pre-fix reproduction's own mechanism no longer applies to the fixed
// code, which is the point.
//
// What replaces it is the same shape TestCloseIfIdleNeverKillsATurnItRaces-
// AgainstConcurrently already uses for CloseIfIdle itself: claim "victim"
// busy synchronously via open() — under the same s.mu CloseIfIdle checks
// Busy under — before racing a real Send() against a real eviction pass, so
// every iteration starts from the exact state that used to be dangerous.
func TestEvictIfFullNeverKillsASessionItRacesAgainstConcurrently(t *testing.T) {
	st := newMemStore()
	sup := New(Config{
		Binary: writeFakeClaude(t, 40*time.Millisecond), WorkdirRoot: t.TempDir(), MaxLive: 1,
		Policies: map[string]Policy{"chat": {Idle: time.Minute, TurnTimeout: 5 * time.Second}},
		Env:      os.Environ(),
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	ctx := context.Background()

	if _, err := sup.Send(ctx, "victim", "chat", "warm up"); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	const iterations = 30
	for i := 0; i < iterations; i++ {
		sess, err := sup.open(ctx, "victim", "chat", sup.policy("chat"), Options{})
		if err != nil {
			t.Fatalf("iteration %d: open victim: %v", i, err)
		}
		newcomer := fmt.Sprintf("newcomer-%d", i)

		var wg sync.WaitGroup
		var sendErr, openErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, sendErr = sess.Send(ctx, "concurrent turn", 5*time.Second, nil)
		}()
		go func() {
			defer wg.Done()
			// MaxLive is 1 and "victim" is already the sole live session, so
			// opening any new key runs evictIfFull with "victim" as its only
			// candidate — exactly the scenario the pre-fix code got wrong.
			_, openErr = sup.open(ctx, newcomer, "chat", sup.policy("chat"), Options{})
		}()
		wg.Wait()

		if sendErr != nil {
			t.Fatalf("iteration %d: evictIfFull killed an in-flight turn on the "+
				"session it was mid-way through evicting: %v", i, sendErr)
		}
		if openErr != nil {
			t.Fatalf("iteration %d: opening the newcomer failed: %v", i, openErr)
		}
		sup.Close(newcomer) // tidy up so MaxLive=1 keeps applying next iteration
	}
}

// --- Reap's own version of the same guarantee -------------------------
//
// F1 named Reap the more dangerous of the two call sites — it runs every
// minute, against every session, not just at a cold start under pressure.
// Same fix (CloseIfIdle instead of a bare Busy-then-Close), same proof: race
// a real Send against a real Reap pass, from a state Reap already considers
// squarely overdue, and confirm it is never won by the wrong side.
func TestReapNeverKillsASessionItRacesAgainstConcurrently(t *testing.T) {
	sup, _ := newRaceSupervisor(t, 40*time.Millisecond)
	ctx := context.Background()

	if _, err := sup.Send(ctx, "reap-race", "chat", "warm up"); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	const iterations = 30
	// Far enough past newRaceSupervisor's one-minute idle window that every
	// live session, however recently active, reads as overdue to Reap.
	farFuture := time.Now().Add(24 * time.Hour)
	for i := 0; i < iterations; i++ {
		sess, err := sup.open(ctx, "reap-race", "chat", sup.policy("chat"), Options{})
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
			sup.Reap(farFuture)
		}()
		wg.Wait()

		if sendErr != nil {
			t.Fatalf("iteration %d: Reap killed an in-flight turn: %v", i, sendErr)
		}
	}
}
