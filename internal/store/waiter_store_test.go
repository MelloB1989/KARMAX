package store

import (
	"sync"
	"testing"
	"time"
)

func TestResolveWaiterIsCompareAndSet(t *testing.T) {
	s := newTestStore(t)
	if err := s.ArmWaiter(Waiter{ID: "w1", ExecutionID: "e1", Step: "await-1", EventKind: "jira.issue.updated"}); err != nil {
		t.Fatalf("arm: %v", err)
	}

	ok, err := s.ResolveWaiter("w1", `{"status":"Prioritized"}`)
	if err != nil || !ok {
		t.Fatalf("first resolve: %v %v", ok, err)
	}

	// Two events arriving at once must not both resume the run — the second
	// resolve has to see it is already done.
	ok, err = s.ResolveWaiter("w1", `{"status":"Prioritized"}`)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if ok {
		t.Error("a waiter was resolved twice")
	}

	result, resolved, err := s.WaiterResult("e1", "await-1")
	if err != nil || !resolved || result != `{"status":"Prioritized"}` {
		t.Fatalf("WaiterResult after resolve: %q %v %v", result, resolved, err)
	}
}

func TestConcurrentResolveHasExactlyOneWinner(t *testing.T) {
	s := newTestStore(t)
	if err := s.ArmWaiter(Waiter{ID: "w1", ExecutionID: "e1", Step: "s"}); err != nil {
		t.Fatal(err)
	}

	const racers = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.ResolveWaiter("w1", "{}")
			if err == nil && ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d resolves won, want exactly 1", wins)
	}
}

func TestArmWaiterIsIdempotentOnStep(t *testing.T) {
	s := newTestStore(t)
	if err := s.ArmWaiter(Waiter{ID: "w1", ExecutionID: "e1", Step: "s", EventKind: "k1"}); err != nil {
		t.Fatalf("first arm: %v", err)
	}
	// A redelivered step re-arming must not disturb the row, and must not blow
	// up on the (execution_id, step) unique index either.
	if err := s.ArmWaiter(Waiter{ID: "w2", ExecutionID: "e1", Step: "s", EventKind: "k2"}); err != nil {
		t.Fatalf("second arm: %v", err)
	}

	pending, err := s.PendingWaiters("k1")
	if err != nil || len(pending) != 1 || pending[0].ID != "w1" {
		t.Fatalf("re-arm changed the stored waiter: %+v, %v", pending, err)
	}
}

func TestWaiterResultBeforeResolution(t *testing.T) {
	s := newTestStore(t)
	_, resolved, err := s.WaiterResult("nope", "nope")
	if err != nil || resolved {
		t.Fatalf("unknown waiter: resolved=%v err=%v", resolved, err)
	}
	if err := s.ArmWaiter(Waiter{ID: "w1", ExecutionID: "e1", Step: "s"}); err != nil {
		t.Fatal(err)
	}
	_, resolved, err = s.WaiterResult("e1", "s")
	if err != nil || resolved {
		t.Fatalf("armed but unresolved: resolved=%v err=%v", resolved, err)
	}
}

func TestPendingWaitersFiltersByEventKind(t *testing.T) {
	s := newTestStore(t)
	if err := s.ArmWaiter(Waiter{ID: "w1", ExecutionID: "e1", Step: "s1", EventKind: "jira.issue.updated"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ArmWaiter(Waiter{ID: "w2", ExecutionID: "e2", Step: "s1", EventKind: "github.pr.merged"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.PendingWaiters("jira.issue.updated")
	if err != nil || len(got) != 1 || got[0].ID != "w1" {
		t.Fatalf("PendingWaiters: %+v, %v", got, err)
	}

	if _, err := s.ResolveWaiter("w1", "{}"); err != nil {
		t.Fatal(err)
	}
	got, err = s.PendingWaiters("jira.issue.updated")
	if err != nil || len(got) != 0 {
		t.Fatalf("a resolved waiter is still pending: %+v", got)
	}
}

func TestExpireWaiters(t *testing.T) {
	s := newTestStore(t)
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)

	if err := s.ArmWaiter(Waiter{ID: "w1", ExecutionID: "e1", Step: "s1", EventKind: "k", ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if err := s.ArmWaiter(Waiter{ID: "w2", ExecutionID: "e2", Step: "s1", EventKind: "k", ExpiresAt: &future}); err != nil {
		t.Fatal(err)
	}
	if err := s.ArmWaiter(Waiter{ID: "w3", ExecutionID: "e3", Step: "s1", EventKind: "k"}); err != nil {
		t.Fatal(err)
	}

	expired, err := s.ExpireWaiters(time.Now())
	if err != nil || len(expired) != 1 || expired[0].ID != "w1" {
		t.Fatalf("ExpireWaiters: %+v, %v", expired, err)
	}

	// An expired waiter must not be double-expired on a second sweep.
	expired, err = s.ExpireWaiters(time.Now())
	if err != nil || len(expired) != 0 {
		t.Fatalf("second sweep re-expired a waiter: %+v", expired)
	}

	// Only the past-deadline waiter left the pending pool.
	pending, err := s.PendingWaiters("k")
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending after expiry: %+v, %v", pending, err)
	}

	// And it is now resolved, with a result available.
	_, resolved, err := s.WaiterResult("e1", "s1")
	if err != nil || !resolved {
		t.Fatalf("expired waiter should read as resolved: resolved=%v err=%v", resolved, err)
	}

	// A waiter already resolved by ResolveWaiter must not later be reported as
	// expired.
	if ok, err := s.ResolveWaiter("w2", "{}"); err != nil || !ok {
		t.Fatal(err)
	}
	expired, err = s.ExpireWaiters(future.Add(time.Hour))
	if err != nil || len(expired) != 0 {
		t.Fatalf("an already-resolved waiter was expired: %+v", expired)
	}
}
