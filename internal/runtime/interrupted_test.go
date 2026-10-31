package runtime

import (
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// The daemon was restarted a minute into a wa-memory run. The reaper only
// looked at runs older than the fifteen-minute lease, and only once, at
// start, so that run stayed "running" for good and was never resumed — and
// its lease, which died with the process, would have refused the resume
// anyway. Anything started before this process came up was cut off by the
// restart, however recently it began.
func TestARunCutOffByARestartIsResumedHoweverRecent(t *testing.T) {
	db := testStore(t)
	booted := time.Now()
	rt := &KarmaxRuntime{store: db, log: zap.NewNop(), startedAt: booted}

	const dead = "run-from-the-last-process"
	if err := db.StartLoopRun(store.LoopRun{ID: dead, Loop: "wa-memory", Attempt: 1,
		StartedAt: booted.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.AcquireLoopLease("wa-memory", dead, leaseTTL); err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	// A run this process has already started is its own and is left alone.
	const mine = "run-from-this-process"
	if err := db.StartLoopRun(store.LoopRun{ID: mine, Loop: "wa-monitor", Attempt: 1,
		StartedAt: booted.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}

	rt.resumeInterruptedRuns()

	runs, _ := db.RecentLoopRuns("wa-memory", 5)
	if len(runs) != 1 || runs[0].Status != "interrupted" {
		t.Fatalf("the cut-off run is %+v, want it marked interrupted", runs)
	}
	due, _ := db.DueLoopRetries(time.Now().Add(retryDelay(1) + time.Second))
	if len(due) != 1 || due[0].Loop != "wa-memory" {
		t.Errorf("due retries = %+v, want the cut-off run resumed", due)
	}
	if ok, _ := db.AcquireLoopLease("wa-memory", "the-resume", leaseTTL); !ok {
		t.Error("the dead run's lease still refuses the loop, so the resume would be skipped")
	}
	if own, _ := db.RecentLoopRuns("wa-monitor", 5); len(own) != 1 || own[0].Status != "running" {
		t.Errorf("this process's own run was touched: %+v", own)
	}
}
