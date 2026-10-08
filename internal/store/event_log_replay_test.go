package store

import (
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

// The 27 Sep incident, reproduced against a real store.
//
// Migration 019 carried the pre-bus events table into event_log. The migration
// list is replayed on every store open, and retention prunes event_log while
// the old table is left alone — so every open after a prune re-inserted the
// pruned history with fresh sequence numbers, and every subscriber read months
// of old messages as new ones. wa-monitor replied to ten of them.

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := New(path, zap.NewNop())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func TestPrunedHistoryIsNotReinsertedOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "karmax.db")
	s := openAt(t, path)

	// A live log with recent activity, as a running daemon has.
	if _, err := s.db.Exec(`INSERT INTO event_log (event_id, kind, payload, created_at)
		VALUES ('live-1', 'comms.message', '{}', ?)`, time.Now().UTC()); err != nil {
		t.Fatalf("seed live event: %v", err)
	}
	// History left behind in the pre-bus table, as on the operator's machine.
	for _, id := range []string{"june-1", "june-2", "july-1"} {
		if _, err := s.db.Exec(`INSERT INTO events (id, kind, payload, created_at)
			VALUES (?, 'comms.message', '{}', '2026-06-23 18:38:22')`, id); err != nil {
			t.Fatalf("seed old event: %v", err)
		}
	}
	headBefore, err := s.LogHead(DefaultWorkspace)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	s.Close()

	// Reopening is what every CLI command and every restart does.
	s = openAt(t, path)
	defer s.Close()

	headAfter, err := s.LogHead(DefaultWorkspace)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if headAfter != headBefore {
		var n int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE seq > ?`, headBefore).Scan(&n)
		t.Fatalf("reopening the store put %d old events on the live log (head %d → %d); "+
			"every subscriber would read them as new", n, headBefore, headAfter)
	}
}

// The carry-over still has a job: a store that has never had a log gets its
// history, once.
func TestHistoryIsCarriedIntoAnEmptyLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "karmax.db")
	s := openAt(t, path)
	if _, err := s.db.Exec(`INSERT INTO events (id, kind, payload, created_at)
		VALUES ('old-1', 'comms.message', '{}', '2026-06-23 18:38:22')`); err != nil {
		t.Fatalf("seed old event: %v", err)
	}
	s.Close()

	s = openAt(t, path)
	defer s.Close()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE event_id = 'old-1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("an empty log should receive the old history once, got %d copies", n)
	}
}
