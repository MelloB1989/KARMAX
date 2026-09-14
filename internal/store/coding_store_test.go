package store

import (
	"testing"
	"time"
)

func TestDeleteCodingSessionsBySessionIDRemovesEveryRowForThatSession(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		if err := s.SaveCodingSession(StoredCodingSession{
			ID: []string{"row-0", "row-1", "row-2"}[i], ToolType: "claude_code",
			SessionID: "lyzn:task-1", AgentID: "a1", Status: "completed",
		}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if err := s.SaveCodingSession(StoredCodingSession{
		ID: "other", ToolType: "claude_code", SessionID: "lyzn:task-2", AgentID: "a1",
	}); err != nil {
		t.Fatalf("save other: %v", err)
	}

	if err := s.DeleteCodingSessionsBySessionID("lyzn:task-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	left, err := s.ListCodingSessions("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 1 || left[0].SessionID != "lyzn:task-2" {
		t.Fatalf("got %v, want only lyzn:task-2 left", left)
	}
}

func TestListStaleCodingSessionIDsFindsOnlyThePrefixAndTheAge(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveCodingSession(StoredCodingSession{ID: "a", ToolType: "claude_code", SessionID: "lyzn:task-1", AgentID: "a1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCodingSession(StoredCodingSession{ID: "b", ToolType: "claude_code", SessionID: "other:task-9", AgentID: "a1"}); err != nil {
		t.Fatal(err)
	}

	none, err := s.ListStaleCodingSessionIDs("lyzn:", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("got %v, want nothing — both rows are fresh, the cutoff is in the past", none)
	}

	stale, err := s.ListStaleCodingSessionIDs("lyzn:", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stale) != 1 || stale[0] != "lyzn:task-1" {
		t.Fatalf("got %v, want exactly [lyzn:task-1] — the other prefix must not match", stale)
	}
}
