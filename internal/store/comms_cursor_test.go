package store

import (
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestCommsCursorNeverRewinds(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "t.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A channel that has never run must be distinguishable from one sitting at
	// the epoch — otherwise a first-ever start replays all of history.
	if _, seen, err := s.CommsCursor("whatsapp-main"); err != nil || seen {
		t.Fatalf("fresh cursor: seen=%v err=%v", seen, err)
	}

	newer := time.Now().Truncate(time.Second)
	older := newer.Add(-time.Hour)

	if err := s.SetCommsCursor("whatsapp-main", newer, "B"); err != nil {
		t.Fatal(err)
	}
	// Replaying an older message must not drag the cursor back, or the same
	// window replays again after every restart.
	if err := s.SetCommsCursor("whatsapp-main", older, "A"); err != nil {
		t.Fatal(err)
	}
	got, seen, err := s.CommsCursor("whatsapp-main")
	if err != nil || !seen {
		t.Fatalf("cursor: seen=%v err=%v", seen, err)
	}
	if !got.Equal(newer.UTC()) {
		t.Errorf("cursor rewound to %v, want %v", got, newer.UTC())
	}
}
