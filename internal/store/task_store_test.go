package store

import (
	"testing"
	"time"
)

// A task exists so work survives the turn that created it. The queue is what
// makes that true: due now, due later, and never lost.
func TestTheQueueReturnsWhatIsDueAndNothingElse(t *testing.T) {
	s := newTestStore(t)

	now, _ := s.CreateTask(Task{Goal: "do it now"})
	later := time.Now().Add(2 * time.Hour)
	_, _ = s.CreateTask(Task{Goal: "do it later", NextActionAt: &later})
	doneTask, _ := s.CreateTask(Task{Goal: "already finished"})
	if err := s.UpdateTask(doneTask.ID, TaskUpdate{Status: TaskDone}); err != nil {
		t.Fatal(err)
	}
	blocked, _ := s.CreateTask(Task{Goal: "waiting on a person"})
	if err := s.UpdateTask(blocked.ID, TaskUpdate{Status: TaskBlocked}); err != nil {
		t.Fatal(err)
	}

	due, err := s.DueTasks(time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != now.ID {
		got := make([]string, 0, len(due))
		for _, d := range due {
			got = append(got, d.Goal)
		}
		t.Fatalf("due = %v, want only the one that is due now", got)
	}
}

// The runner must not be able to take on everything at once: a dozen tasks
// coming due together would be a dozen model calls in one tick.
func TestTheQueueIsBounded(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 10; i++ {
		if _, err := s.CreateTask(Task{Goal: "work"}); err != nil {
			t.Fatal(err)
		}
	}
	due, err := s.DueTasks(time.Now(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 3 {
		t.Errorf("took %d tasks in one tick, want 3", len(due))
	}
}

// A task claimed by a process that then died must come back. Left as it was, it
// reads as busy forever and is never picked up again — the work is simply lost,
// which is the whole thing a task is supposed to prevent.
func TestWorkAbandonedByADeadDaemonComesBack(t *testing.T) {
	s := newTestStore(t)
	task, _ := s.CreateTask(Task{Goal: "long job"})

	// Claimed, with its next action pushed out — then the daemon dies.
	future := time.Now().Add(30 * time.Minute)
	if err := s.UpdateTask(task.ID, TaskUpdate{Status: TaskWorking, NextActionAt: &future}); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueTasks(time.Now(), 10); len(due) != 0 {
		t.Fatal("precondition: a claimed task should not be due")
	}

	n, err := s.ReleaseStuckTasks(time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("released %d tasks, want 1", n)
	}
	due, _ := s.DueTasks(time.Now(), 10)
	if len(due) != 1 || due[0].ID != task.ID {
		t.Error("the abandoned task did not come back onto the queue")
	}
}

// Releasing must not disturb work that is genuinely in flight.
func TestReleasingLeavesLiveWorkAlone(t *testing.T) {
	s := newTestStore(t)
	task, _ := s.CreateTask(Task{Goal: "running right now"})
	future := time.Now().Add(30 * time.Minute)
	if err := s.UpdateTask(task.ID, TaskUpdate{Status: TaskWorking, NextActionAt: &future}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.ReleaseStuckTasks(time.Now().Add(-time.Hour)); n != 0 {
		t.Errorf("released %d live tasks", n)
	}
}

// A round that succeeds must clear the failure the round before it recorded, or
// a finished task carries a stale error forever.
func TestASucceedingRoundClearsTheOldError(t *testing.T) {
	s := newTestStore(t)
	task, _ := s.CreateTask(Task{Goal: "flaky"})
	if err := s.UpdateTask(task.ID, TaskUpdate{Status: TaskWorking, LastError: "timed out"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(task.ID, TaskUpdate{Status: TaskDone, Progress: "finished"}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetTask(task.ID)
	if got.LastError != "" {
		t.Errorf("a completed task still reports %q", got.LastError)
	}
}

// Rounds are counted so a task that cannot converge is eventually given up on
// rather than running until the account's quota is gone.
func TestRoundsAreCounted(t *testing.T) {
	s := newTestStore(t)
	task, _ := s.CreateTask(Task{Goal: "count me"})
	for i := 0; i < 3; i++ {
		if err := s.UpdateTask(task.ID, TaskUpdate{Status: TaskWorking, BumpAttempt: true}); err != nil {
			t.Fatal(err)
		}
	}
	got, _, _ := s.GetTask(task.ID)
	if got.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", got.Attempts)
	}
}

// What the operator has already been told is kept, so a task that ticks twenty
// times without changing does not send twenty messages.
func TestASilentRoundKeepsWhatTheOperatorWasLastTold(t *testing.T) {
	s := newTestStore(t)
	task, _ := s.CreateTask(Task{Goal: "chatty"})
	if err := s.UpdateTask(task.ID, TaskUpdate{Reported: "started on it"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(task.ID, TaskUpdate{Status: TaskWorking, Progress: "more work"}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetTask(task.ID)
	if got.Reported != "started on it" {
		t.Errorf("reported = %q; a silent round erased what they were told", got.Reported)
	}
}

// A task that failed must stay visible. Pruning it away leaves the operator
// with no record that something they asked for never happened.
func TestPruningKeepsFailures(t *testing.T) {
	s := newTestStore(t)
	done, _ := s.CreateTask(Task{Goal: "finished"})
	failed, _ := s.CreateTask(Task{Goal: "never happened"})
	_ = s.UpdateTask(done.ID, TaskUpdate{Status: TaskDone})
	_ = s.UpdateTask(failed.ID, TaskUpdate{Status: TaskFailed})

	if _, err := s.PruneTasks(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetTask(failed.ID); !ok {
		t.Error("a failed task was pruned; nothing records that it never happened")
	}
	if _, ok, _ := s.GetTask(done.ID); ok {
		t.Error("a finished task was kept")
	}
}
