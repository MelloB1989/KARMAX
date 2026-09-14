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

func TestGetSessionKeyReturnsEmptyForAnUnmintedKey(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetSessionKey("lyzn:task-never-run")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty — nothing has been minted for this key yet", got)
	}
}

func TestSaveSessionKeyThenGetSessionKeyRoundTrips(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveSessionKey("lyzn:task-1", "11111111-1111-1111-1111-111111111111", "claude_code"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.GetSessionKey("lyzn:task-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("got %q, want the saved uuid", got)
	}
}

// TestSaveSessionKeyOverwritesAnExistingMapping is the shape a stale-mapping
// recovery needs: dropping a mapping and minting a fresh one for the same
// key must leave the NEW uuid in place, not a stale duplicate row.
func TestSaveSessionKeyOverwritesAnExistingMapping(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveSessionKey("lyzn:task-1", "11111111-1111-1111-1111-111111111111", "claude_code"); err != nil {
		t.Fatalf("save first: %v", err)
	}
	if err := s.SaveSessionKey("lyzn:task-1", "22222222-2222-2222-2222-222222222222", "claude_code"); err != nil {
		t.Fatalf("save second: %v", err)
	}
	got, err := s.GetSessionKey("lyzn:task-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("got %q, want the second, overwriting uuid", got)
	}
}

func TestDeleteSessionKeyRemovesTheMapping(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveSessionKey("lyzn:task-1", "11111111-1111-1111-1111-111111111111", "claude_code"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.DeleteSessionKey("lyzn:task-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := s.GetSessionKey("lyzn:task-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty after delete", got)
	}
}

// TestDeleteSessionKeyOnAnUnknownKeyIsNotAnError matches the rest of this
// package's cleanup methods: removing something already gone is success, not
// a failure — Cleanup and the six-hour sweep both rely on that.
func TestDeleteSessionKeyOnAnUnknownKeyIsNotAnError(t *testing.T) {
	s := newTestStore(t)
	if err := s.DeleteSessionKey("lyzn:never-existed"); err != nil {
		t.Fatalf("delete of an unknown key returned an error: %v", err)
	}
}
