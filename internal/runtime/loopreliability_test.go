package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

func loopTestRuntime(t *testing.T) *KarmaxRuntime {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "r.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &KarmaxRuntime{
		store: db, log: zap.NewNop(), cfg: &config.KarmaxConfig{},
		memory:    memory.NewFactory(t.TempDir(), db, zap.NewNop()),
		startedAt: time.Now(),
		pending:   newPendingTriggers(),
	}
}

// ---- E1: leases left by a dead process ----

func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run true: %v", err)
	}
	return cmd.Process.Pid
}

func TestLeaseHolderLiveness(t *testing.T) {
	ours := leaseOwnerFor("run")
	if !leaseHolderAlive(ours) {
		t.Error("this process's own lease must be alive")
	}
	if leaseHolderAlive("some-legacy-uuid") {
		t.Error("an owner not in this build's format cannot be a live holder")
	}
	other := func(pid int, host string) string {
		return "other-instance|" + itoa(pid) + "|" + host + "|run"
	}
	if leaseHolderAlive(other(os.Getpid(), processHost)) {
		t.Error("our pid under a different instance is a reused pid, not a live holder")
	}
	if !leaseHolderAlive(other(os.Getppid(), processHost)) {
		t.Error("a live concurrent process's lease must not be stolen")
	}
	if leaseHolderAlive(other(exitedPID(t), processHost)) {
		t.Error("an exited process's lease should be dead")
	}
	if !leaseHolderAlive(other(exitedPID(t), "another-machine")) {
		t.Error("a lease from another host cannot be judged, so it is kept")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestStartupClearsDeadLeasesAndReapsTheirRunsButNotALiveProcesses(t *testing.T) {
	rt := loopTestRuntime(t)
	rt.startedAt = time.Now().Add(time.Minute) // everything below predates this "start"
	dead := "dead-instance|" + itoa(exitedPID(t)) + "|" + processHost + "|run-dead"
	live := "live-instance|" + itoa(os.Getppid()) + "|" + processHost + "|run-live"

	for _, c := range []struct{ loop, owner, run string }{
		{"dead-loop", dead, "run-dead"}, {"live-loop", live, "run-live"},
	} {
		if ok, err := rt.store.AcquireLoopLease(c.loop, c.owner, leaseTTL); err != nil || !ok {
			t.Fatal(ok, err)
		}
		// Minutes old: well inside the TTL, so the old age-based reap missed it.
		if err := rt.store.StartLoopRun(store.LoopRun{ID: c.run, Loop: c.loop, Attempt: 1,
			StartedAt: time.Now().Add(-3 * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}

	rt.resumeInterruptedRuns()

	if ok, _ := rt.store.AcquireLoopLease("dead-loop", "next", time.Minute); !ok {
		t.Error("the dead process's lease should be gone at once")
	}
	if ok, _ := rt.store.AcquireLoopLease("live-loop", "thief", time.Minute); ok {
		t.Error("a live concurrent process's lease was stolen")
	}
	if runs, _ := rt.store.RecentLoopRuns("dead-loop", 5); len(runs) != 1 || runs[0].Status != "interrupted" {
		t.Errorf("dead run = %+v, want interrupted", runs)
	}
	if runs, _ := rt.store.RecentLoopRuns("live-loop", 5); len(runs) != 1 || runs[0].Status != "running" {
		t.Errorf("live run = %+v, want untouched", runs)
	}
	if p, _ := rt.store.LoopRetryPending("dead-loop"); !p {
		t.Error("the interrupted run should be scheduled to resume")
	}
}

// ---- E3: a cron tick must not trample a pending retry ----

func TestCronTickDoesNotRestartAFailedRunsCheckpoints(t *testing.T) {
	rt := loopTestRuntime(t)
	var step1Runs, step2Runs atomic.Int32
	failStep2 := atomic.Bool{}
	failStep2.Store(true)

	l := loopkit.Loop{Name: "ticker", Run: func(_ context.Context, k loopkit.Kit) error {
		if _, err := k.Step("one", func() (string, error) { step1Runs.Add(1); return "x", nil }); err != nil {
			return err
		}
		_, err := k.Step("two", func() (string, error) {
			step2Runs.Add(1)
			if failStep2.Load() {
				return "", errors.New("boom")
			}
			return "y", nil
		})
		return err
	}}
	ctx := context.Background()
	tick := loopkit.Trigger{Kind: loopkit.TriggerSchedule}

	rt.runLoopDurable(ctx, l, tick, 1) // fails at step two; retry scheduled
	if p, _ := rt.store.LoopRetryPending("ticker"); !p {
		t.Fatal("retry should be pending after the failure")
	}

	failStep2.Store(false)
	rt.runLoopDurable(ctx, l, tick, 1) // the next cron tick, retry not yet run
	if step1Runs.Load() != 1 || step2Runs.Load() != 1 {
		t.Fatalf("the tick ran the loop (step1=%d step2=%d); it must be skipped while a retry is pending",
			step1Runs.Load(), step2Runs.Load())
	}

	rt.runLoopDurable(ctx, l, loopkit.Trigger{Kind: "retry"}, 2) // the retry worker's dispatch
	if step1Runs.Load() != 1 {
		t.Errorf("step one re-executed on retry (%d runs): its checkpoint was lost", step1Runs.Load())
	}
	if step2Runs.Load() != 2 {
		t.Errorf("step two runs = %d, want 2", step2Runs.Load())
	}
	if p, _ := rt.store.LoopRetryPending("ticker"); p {
		t.Error("retry should be consumed")
	}

	// With nothing pending, ticks run normally again.
	rt.runLoopDurable(ctx, l, tick, 1)
	if step1Runs.Load() != 2 {
		t.Errorf("a tick with no retry pending must run: step1=%d", step1Runs.Load())
	}
}

func TestRetryThatCannotGetTheLeaseStaysPending(t *testing.T) {
	rt := loopTestRuntime(t)
	l := loopkit.Loop{Name: "busy", Run: func(context.Context, loopkit.Kit) error { return errors.New("x") }}
	rt.runLoopDurable(context.Background(), l, loopkit.Trigger{Kind: loopkit.TriggerSchedule}, 1)
	if ok, _ := rt.store.AcquireLoopLease("busy", leaseOwnerFor("other"), time.Minute); !ok {
		t.Fatal("setup")
	}
	rt.runLoopDurable(context.Background(), l, loopkit.Trigger{Kind: "retry"}, 2)
	if p, _ := rt.store.LoopRetryPending("busy"); !p {
		t.Error("a retry that lost the lease race must stay pending, not vanish")
	}
}
