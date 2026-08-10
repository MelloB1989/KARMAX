package store

import (
	"testing"
	"time"
)

func TestACompletedStepIsRememberedForTheNextAttempt(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveLoopStep("exec-1", "digest", "summarise", "today was quiet"); err != nil {
		t.Fatal(err)
	}

	result, done, err := s.LoopStep("exec-1", "summarise")
	if err != nil || !done || result != "today was quiet" {
		t.Fatalf("step = %q, done = %v, err = %v", result, done, err)
	}

	if _, done, _ := s.LoopStep("exec-1", "send"); done {
		t.Error("a step that never ran reported as done")
	}
	// Checkpoints belong to one execution, not to the loop.
	if _, done, _ := s.LoopStep("exec-2", "summarise"); done {
		t.Error("a different execution saw another one's checkpoints")
	}
}

func TestFinishingAnExecutionDropsItsCheckpoints(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"a", "b", "c"} {
		if err := s.SaveLoopStep("exec-1", "l", name, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveLoopStep("exec-2", "l", "a", ""); err != nil {
		t.Fatal(err)
	}

	n, err := s.CompletedSteps("exec-1")
	if err != nil || n != 3 {
		t.Fatalf("completed = %d, err = %v", n, err)
	}
	if err := s.ClearLoopSteps("exec-1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CompletedSteps("exec-1"); n != 0 {
		t.Errorf("%d checkpoints survived", n)
	}
	if n, _ := s.CompletedSteps("exec-2"); n != 1 {
		t.Error("clearing one execution took another's checkpoints with it")
	}
}

func TestTheExecutionCarriesAcrossAttempts(t *testing.T) {
	s := newTestStore(t)
	if got, err := s.LoopExecution("never-run"); err != nil || got != "" {
		t.Fatalf("execution = %q, err = %v", got, err)
	}
	if err := s.SetLoopExecution("l", "exec-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LoopExecution("l"); got != "exec-1" {
		t.Errorf("execution = %q", got)
	}

	// Setting it must not disturb the lease and counters in the same row.
	if _, err := s.AcquireLoopLease("l", "owner", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLoopExecution("l", "exec-2"); err != nil {
		t.Fatal(err)
	}
	states, _ := s.LoopStates()
	if len(states) != 1 || states[0].LeaseOwner != "owner" {
		t.Errorf("the lease was lost: %+v", states)
	}
}

func TestOrphanedCheckpointsArePruned(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveLoopStep("exec-1", "l", "a", ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PruneLoopSteps(time.Now().Add(-time.Hour)); n != 0 {
		t.Errorf("pruned %d fresh checkpoints", n)
	}
	if n, _ := s.PruneLoopSteps(time.Now().Add(time.Hour)); n != 1 {
		t.Error("a stale checkpoint was not pruned")
	}
}
