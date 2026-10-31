package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeSlowLoggingClaude answers each line after delay, and appends every line
// it is given to a file, so a test can tell what was actually written to it.
func writeSlowLoggingClaude(t *testing.T, delay time.Duration) (bin, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "lines")
	bin = writeScript(t, "while IFS= read -r line; do echo \"$line\" >> "+log+"\nsleep "+
		fmt.Sprintf("%g", delay.Seconds())+"\n"+resultLine+"done\n")
	return bin, log
}

func linesIn(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

// Two conversations reach the orchestrator's one session at once: an
// operator's message while a fleet event is being handled. The second used to
// queue on the session's lock with no regard for its own deadline, then write
// its message anyway once the lock came free — with its time already gone —
// and the turn it had just started was killed with the session.
//
// A caller waits for the turn ahead of it no longer than it would wait out a
// turn the session started by itself, and then it is busy: nothing written,
// nothing killed.
func TestASecondCallerGivesUpWithoutWritingOrKillingTheSession(t *testing.T) {
	bin, lines := writeSlowLoggingClaude(t, 1500*time.Millisecond)
	st := newMemStore()
	sup := New(Config{
		Binary: bin, WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		Policies: map[string]Policy{"orch": {TurnTimeout: 5 * time.Second}},
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)

	first := make(chan error, 1)
	go func() {
		_, err := sup.Send(context.Background(), "orch", "orch", "fleet event")
		first <- err
	}()
	// Until the first turn is written and in flight.
	for deadline := time.Now().Add(3 * time.Second); linesIn(t, lines) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the first turn never reached the session")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := sup.Send(ctx, "orch", "orch", "operator message")
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("second caller: err = %v, want ErrSessionBusy", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("second caller waited %s past its own 200ms deadline", waited)
	}

	if err := <-first; err != nil {
		t.Fatalf("the first turn was lost: %v", err)
	}
	if n := linesIn(t, lines); n != 1 {
		t.Errorf("%d lines reached the session; the caller that gave up must write nothing", n)
	}
	if !slices.Equal(sup.Live(), []string{"orch"}) {
		t.Errorf("live = %v: the session was killed for a caller that never used it", sup.Live())
	}
}

// A caller whose time is already gone by the time the session is free writes
// nothing. Writing it anyway started a turn nobody would read, and the error
// that followed killed the session mid-turn.
func TestNothingIsWrittenOnceTheCallersTimeIsGone(t *testing.T) {
	bin, lines := writeSlowLoggingClaude(t, 10*time.Millisecond)
	st := newMemStore()
	sup := New(Config{
		Binary: bin, WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		Policies: map[string]Policy{"orch": {TurnTimeout: 5 * time.Second}},
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	if _, err := sup.Send(context.Background(), "orch", "orch", "warm up"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sup.Send(ctx, "orch", "orch", "too late"); err == nil {
		t.Fatal("a turn with no time left reported success")
	}
	time.Sleep(100 * time.Millisecond)
	if n := linesIn(t, lines); n != 1 {
		t.Errorf("%d lines reached the session; one written after the caller's time ran out", n)
	}
	if !slices.Equal(sup.Live(), []string{"orch"}) {
		t.Errorf("live = %v: the session was killed for a turn that never started", sup.Live())
	}
	if _, err := sup.Send(context.Background(), "orch", "orch", "next"); err != nil {
		t.Errorf("the session could not take the next turn: %v", err)
	}
}

// TurnBudget is the longest SendWith can take: waiting out the turn ahead,
// the turn itself, and starting the process. A caller that bounds the call
// with less cuts off turns the kind's own timeout still allows.
func TestTurnBudgetCoversTheWaitAndTheTurn(t *testing.T) {
	sup := New(Config{
		Binary: "claude", WorkdirRoot: t.TempDir(),
		Policies: map[string]Policy{"orch": {TurnTimeout: 3 * time.Minute}},
	}, newMemStore(), NewBreaker(0.9, nil), testLog{t}, nil)
	if got, want := sup.TurnBudget("orch"), 6*time.Minute+spawnGrace; got != want {
		t.Errorf("TurnBudget(orch) = %s, want %s", got, want)
	}
	// An unconfigured kind is bounded by the default turn timeout.
	if got, want := sup.TurnBudget("unheard-of"), 4*time.Minute+spawnGrace; got != want {
		t.Errorf("TurnBudget(unheard-of) = %s, want %s", got, want)
	}
}
