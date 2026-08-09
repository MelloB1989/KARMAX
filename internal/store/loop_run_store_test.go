package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "test.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func TestOnlyOneRunHoldsTheLease(t *testing.T) {
	s := newTestStore(t)

	got, err := s.AcquireLoopLease("wa-monitor", "run-1", time.Minute)
	if err != nil || !got {
		t.Fatalf("first acquire: %v %v", got, err)
	}
	// The second run must stand down. This is what stops a 4-minute run on a
	// 2-minute schedule from piling up — which is how one WhatsApp message got
	// answered twice.
	got, err = s.AcquireLoopLease("wa-monitor", "run-2", time.Minute)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if got {
		t.Error("two runs of the same loop both took the lease")
	}
	// A different loop is unaffected.
	if got, _ := s.AcquireLoopLease("hot-sync", "run-3", time.Minute); !got {
		t.Error("a different loop should be able to run concurrently")
	}

	if err := s.ReleaseLoopLease("wa-monitor", "run-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got, _ := s.AcquireLoopLease("wa-monitor", "run-4", time.Minute); !got {
		t.Error("the lease should be free after release")
	}
}

func TestLeaseIsNotReleasedByAStaleOwner(t *testing.T) {
	s := newTestStore(t)
	// A run that overran its lease must not release the lease its successor has
	// since taken, or two runs end up in flight with neither holding anything.
	if _, err := s.AcquireLoopLease("l", "old", -time.Second); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AcquireLoopLease("l", "new", time.Minute); !got {
		t.Fatal("an expired lease should be reclaimable")
	}
	if err := s.ReleaseLoopLease("l", "old"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AcquireLoopLease("l", "third", time.Minute); got {
		t.Error("the stale owner released a lease it no longer held")
	}
}

func TestExpiredLeaseIsReclaimable(t *testing.T) {
	s := newTestStore(t)
	// A crashed process must not wedge a loop closed forever, which is why the
	// lease has an expiry rather than being a boolean.
	if _, err := s.AcquireLoopLease("stuck", "dead-process", -time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := s.AcquireLoopLease("stuck", "live-process", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("an expired lease left the loop permanently blocked")
	}
}

func TestConcurrentAcquireHasExactlyOneWinner(t *testing.T) {
	s := newTestStore(t)
	const racers = 12

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := s.AcquireLoopLease("contended", string(rune('a'+i)), time.Minute)
			if err == nil && ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	// Checking then writing would let several readers all see "free". The
	// conditional upsert is what makes this exactly one.
	if wins != 1 {
		t.Errorf("%d runs took the lease, want exactly 1", wins)
	}
}

func TestRunOutcomeUpdatesStanding(t *testing.T) {
	s := newTestStore(t)
	start := time.Now()

	if err := s.StartLoopRun(LoopRun{
		ID: "r1", Loop: "memory-merge", Trigger: "schedule", Attempt: 1, StartedAt: start,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	retry := start.Add(time.Minute)
	if err := s.FinishLoopRun("r1", "memory-merge", "failed", "gateway down", 1, &retry); err != nil {
		t.Fatalf("finish: %v", err)
	}

	states, err := s.LoopStates()
	if err != nil || len(states) != 1 {
		t.Fatalf("states: %v %v", states, err)
	}
	if states[0].ConsecFails != 1 || states[0].NextRetryAt == nil {
		t.Errorf("a failure should record a retry: %+v", states[0])
	}

	// The retry must be visible to the worker, or it never fires.
	due, err := s.DueLoopRetries(retry.Add(time.Second))
	if err != nil || len(due) != 1 {
		t.Fatalf("due retries: %v %v", due, err)
	}

	// Success clears the counters — otherwise a loop that recovered is reported
	// as broken forever.
	if err := s.StartLoopRun(LoopRun{
		ID: "r2", Loop: "memory-merge", Attempt: 2, StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishLoopRun("r2", "memory-merge", "ok", "", 2, nil); err != nil {
		t.Fatal(err)
	}
	states, _ = s.LoopStates()
	if states[0].ConsecFails != 0 || states[0].NextRetryAt != nil || states[0].LastSuccessAt == nil {
		t.Errorf("success did not clear the failure state: %+v", states[0])
	}
}

func TestDeadRunsSurvivePruning(t *testing.T) {
	s := newTestStore(t)
	old := time.Now().AddDate(0, 0, -30)

	for _, r := range []struct{ id, status string }{{"ok1", "ok"}, {"dead1", "dead"}} {
		if err := s.StartLoopRun(LoopRun{ID: r.id, Loop: "l", Attempt: 1, StartedAt: old}); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishLoopRun(r.id, "l", r.status, "", 1, nil); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.PruneLoopRuns(time.Now().AddDate(0, 0, -14)); err != nil {
		t.Fatal(err)
	}
	// Dead runs are the record of work that never happened. Ageing them out
	// would quietly erase the evidence that something was lost.
	dead, err := s.DeadLoopRuns(10)
	if err != nil || len(dead) != 1 {
		t.Fatalf("dead runs after prune: %v %v", dead, err)
	}
	all, _ := s.RecentLoopRuns("l", 10)
	if len(all) != 1 {
		t.Errorf("the successful old run should have been pruned, got %d rows", len(all))
	}
}

func TestInterruptedRunsAreReaped(t *testing.T) {
	s := newTestStore(t)
	// A killed daemon leaves rows that say "running" forever, which makes the
	// lease look held and the status page look busy.
	if err := s.StartLoopRun(LoopRun{
		ID: "orphan", Loop: "l", Attempt: 1, StartedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReapStaleLoopRuns(time.Now().Add(-30 * time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("reaped %d: %v", n, err)
	}
	dead, _ := s.DeadLoopRuns(10)
	if len(dead) != 1 {
		t.Fatal("the interrupted run was not marked dead")
	}
}
