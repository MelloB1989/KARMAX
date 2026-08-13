package store

import (
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

func turnStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "t.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func turn(id, event string) AgentTurn {
	return AgentTurn{ID: id, AgentID: "nexus", EventID: event, EventKind: "comms.message", Attempt: 1}
}

func TestTurnIsClaimedOnceThenFinished(t *testing.T) {
	s := turnStore(t)
	got, err := s.StartAgentTurn(turn("t1", "evt-1"))
	if err != nil || !got {
		t.Fatalf("first start should claim the turn: %v %v", got, err)
	}
	// A duplicate delivery of a RUNNING turn must not open a second one.
	again, err := s.StartAgentTurn(turn("t2", "evt-1"))
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Error("a duplicate delivery claimed a turn that was already running")
	}
	if err := s.FinishAgentTurn("evt-1", TurnOK, ""); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.RecentTurns("nexus", 10)
	if len(rows) != 1 || rows[0].Status != TurnOK {
		t.Errorf("turns = %+v", rows)
	}
}

// The crash case: a turn left running is reaped and becomes retryable.
func TestStaleRunningTurnsAreReaped(t *testing.T) {
	s := turnStore(t)
	old := turn("t1", "evt-old")
	old.StartedAt = time.Now().Add(-30 * time.Minute)
	if _, err := s.StartAgentTurn(old); err != nil {
		t.Fatal(err)
	}
	reaped, err := s.ReapStaleTurns(time.Now().Add(-5*time.Minute), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].Status != TurnInterrupted {
		t.Fatalf("reaped = %+v", reaped)
	}
	if reaped[0].EventKind != "comms.message" {
		t.Errorf("the event kind must survive for the retry: %+v", reaped[0])
	}
	// And an interrupted turn can then be re-claimed, with the attempt bumped.
	ok, err := s.StartAgentTurn(turn("t1", "evt-old"))
	if err != nil || !ok {
		t.Fatalf("an interrupted turn should be retryable: %v %v", ok, err)
	}
	rows, _ := s.RecentTurns("nexus", 5)
	if rows[0].Attempt != 2 {
		t.Errorf("attempt = %d, want 2", rows[0].Attempt)
	}
}

// A poisonous event must stop resurrecting itself once it has burned its
// attempts, or every boot replays the same crash.
func TestExhaustedTurnsGoDeadNotInterrupted(t *testing.T) {
	s := turnStore(t)
	tn := turn("t1", "evt-poison")
	tn.Attempt = 3
	tn.StartedAt = time.Now().Add(-time.Hour)
	if _, err := s.StartAgentTurn(tn); err != nil {
		t.Fatal(err)
	}
	reaped, err := s.ReapStaleTurns(time.Now().Add(-time.Minute), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].Status != TurnDead {
		t.Fatalf("an exhausted turn should be dead: %+v", reaped)
	}
	// Dead turns are not re-claimable.
	if ok, _ := s.StartAgentTurn(turn("t1", "evt-poison")); ok {
		t.Error("a dead turn was re-claimed")
	}
}

// A turn that finished normally is not reaped by a later restart.
func TestFinishedTurnsAreNotReaped(t *testing.T) {
	s := turnStore(t)
	tn := turn("t1", "evt-done")
	tn.StartedAt = time.Now().Add(-time.Hour)
	if _, err := s.StartAgentTurn(tn); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAgentTurn("evt-done", TurnOK, ""); err != nil {
		t.Fatal(err)
	}
	reaped, _ := s.ReapStaleTurns(time.Now(), 3)
	if len(reaped) != 0 {
		t.Errorf("a finished turn was reaped: %+v", reaped)
	}
}

func TestPruneKeepsDeadTurns(t *testing.T) {
	s := turnStore(t)
	for _, e := range []string{"a", "b"} {
		tn := turn("id-"+e, "evt-"+e)
		tn.StartedAt = time.Now().Add(-48 * time.Hour)
		s.StartAgentTurn(tn)
	}
	s.FinishAgentTurn("evt-a", TurnOK, "")
	s.FinishAgentTurn("evt-b", TurnDead, "gave up")
	if _, err := s.PruneAgentTurns(time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.RecentTurns("nexus", 10)
	if len(rows) != 1 || rows[0].Status != TurnDead {
		t.Errorf("prune should keep dead turns only: %+v", rows)
	}
}

func TestRetryClearsThePreviousFinishTime(t *testing.T) {
	s := turnStore(t)
	if _, err := s.StartAgentTurn(turn("t1", "evt-retry")); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.FinishAgentTurn("evt-retry", TurnFailed, "boom"); err != nil {
		t.Fatalf("finish: %v", err)
	}

	// A failed turn is eligible to be retried, and the retry must not inherit
	// the finish timestamp of the attempt that failed.
	claimed, err := s.StartAgentTurn(turn("t2", "evt-retry"))
	if err != nil || !claimed {
		t.Fatalf("a failed turn should be reclaimable: %v %v", claimed, err)
	}

	var status string
	var finished, started interface{}
	row := s.db.QueryRow(`SELECT status, finished_at, started_at FROM agent_turns WHERE event_id = ?`, "evt-retry")
	if err := row.Scan(&status, &finished, &started); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "running" {
		t.Errorf("status = %q, want running", status)
	}
	if finished != nil {
		t.Errorf("a running turn must not carry a finish time, got %v", finished)
	}
}
