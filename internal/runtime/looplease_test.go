package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// loopRuntime is just enough of a runtime to run loops durably.
func loopRuntime(t *testing.T) *KarmaxRuntime {
	t.Helper()
	db := testStore(t)
	return &KarmaxRuntime{
		store: db, log: zap.NewNop(), cfg: &config.KarmaxConfig{},
		memory:           memory.NewFactory(t.TempDir(), db, zap.NewNop()),
		pending:          newPendingTriggers(),
		loopDefaultAgent: "agent-1",
	}
}

// shortRuns makes a run's deadline and lease milliseconds long for one test.
func shortRuns(t *testing.T, timeout, lease time.Duration) {
	t.Helper()
	wasTimeout, wasLease := loopRunTimeout, leaseTTL
	loopRunTimeout, leaseTTL = timeout, lease
	t.Cleanup(func() { loopRunTimeout, leaseTTL = wasTimeout, wasLease })
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A run that outlives its deadline still holds its loop: memory-merge ran
// nineteen minutes on a fourteen-minute limit, past its fifteen-minute lease,
// and any trigger after minute fifteen would have started a second copy
// beside it. However a loop overruns, a second copy never starts while the
// first is in flight — and once it returns, the loop runs again.
func TestARunPastItsDeadlineKeepsItsLease(t *testing.T) {
	shortRuns(t, 30*time.Millisecond, 90*time.Millisecond)
	rt := loopRuntime(t)

	var inFlight, most, runs atomic.Int32
	release := make(chan struct{})
	l := loopkit.Loop{Name: "slow-pass", Run: func(ctx context.Context, k loopkit.Kit) error {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		runs.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		// Stuck in a call that does not take the run's context.
		<-release
		return nil
	}}
	run := func() chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			rt.runLoopDurable(context.Background(), l, loopkit.Trigger{Kind: loopkit.TriggerSchedule}, 1)
		}()
		return done
	}

	first := run()
	waitUntil(t, "the first run to start", func() bool { return runs.Load() == 1 })
	// Well past the deadline and three leases long.
	time.Sleep(4 * leaseTTL)

	second := run()
	select {
	case <-second:
	case <-time.After(300 * time.Millisecond):
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("a second copy started while the first was still in flight (%d runs)", got)
	}

	close(release)
	for _, done := range []chan struct{}{first, second} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a run did not return")
		}
	}
	if m := most.Load(); m != 1 {
		t.Errorf("%d copies of the loop ran at once", m)
	}

	before := runs.Load()
	<-run()
	if runs.Load() != before+1 {
		t.Error("the loop did not run again once the overrunning run returned")
	}
}
