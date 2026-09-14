package runtime

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func TestPruneStaleLyznSessionsDeletesMatchingRows(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "test.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.SaveCodingSession(store.StoredCodingSession{
		ID: "row-1", ToolType: "claude_code", SessionID: "lyzn:task-9", AgentID: "a1",
	}); err != nil {
		t.Fatalf("save lyzn row: %v", err)
	}
	if err := s.SaveCodingSession(store.StoredCodingSession{
		ID: "row-2", ToolType: "claude_code", SessionID: "other:task-1", AgentID: "a1",
	}); err != nil {
		t.Fatalf("save other row: %v", err)
	}

	rt := &KarmaxRuntime{store: s, log: zap.NewNop()}
	rt.pruneStaleLyznSessions(time.Now().Add(time.Hour)) // cutoff in the future: both rows already qualify by age

	left, err := s.ListCodingSessions("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 1 || left[0].SessionID != "other:task-1" {
		t.Fatalf("got %v, want only the non-lyzn row left", left)
	}
}

// backdateCodingSession sets a row's updated_at directly, bypassing
// SaveCodingSession's own datetime('now'), so a test can stand in for a
// session touched a chosen number of days ago rather than one touched just
// now.
func backdateCodingSession(t *testing.T, s *store.Store, id string, when time.Time) {
	t.Helper()
	if _, err := s.DB().Exec(`UPDATE coding_sessions SET updated_at = ? WHERE id = ?`, when.UTC(), id); err != nil {
		t.Fatalf("backdate %s: %v", id, err)
	}
}

// TestPruneStaleLyznSessionsRespectsTheSevenDayCutoff is what
// TestPruneStaleLyznSessionsDeletesMatchingRows cannot catch: that test's
// cutoff is in the future, so both rows already qualify by age no matter what
// the real cutoff computation does — a reviewer once widened the six-hour
// sweep's cutoff from time.Now().AddDate(0, 0, -7) to time.Now(), which would
// reap every live, blocked-awaiting-an-operator session on the next tick, and
// that suite stayed green throughout.
//
// This test calls lyznSessionStaleCutoff — the exact function retryWorker
// calls in production — against two rows of known, different ages, so
// dropping or widening the cutoff there is a failing test, not a silent
// change.
func TestPruneStaleLyznSessionsRespectsTheSevenDayCutoff(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "test.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Now()

	// Touched 6 days ago: inside the sweep's 7-day window. A live session
	// still waiting on an operator's answer looks exactly like this, and
	// must survive.
	if err := s.SaveCodingSession(store.StoredCodingSession{
		ID: "live", ToolType: "claude_code", SessionID: "lyzn:task-live", AgentID: "a1",
	}); err != nil {
		t.Fatalf("save live row: %v", err)
	}
	backdateCodingSession(t, s, "live", now.AddDate(0, 0, -6))

	// Touched 8 days ago: past the window, and exactly what the sweep exists
	// to reclaim.
	if err := s.SaveCodingSession(store.StoredCodingSession{
		ID: "stale", ToolType: "claude_code", SessionID: "lyzn:task-stale", AgentID: "a1",
	}); err != nil {
		t.Fatalf("save stale row: %v", err)
	}
	backdateCodingSession(t, s, "stale", now.AddDate(0, 0, -8))

	rt := &KarmaxRuntime{store: s, log: zap.NewNop()}
	rt.pruneStaleLyznSessions(lyznSessionStaleCutoff(now))

	left, err := s.ListCodingSessions("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 1 || left[0].SessionID != "lyzn:task-live" {
		t.Fatalf("got %v, want only the 6-day-old (live) row left — the 7-day cutoff must spare it and reap only the 8-day-old row", left)
	}
}
