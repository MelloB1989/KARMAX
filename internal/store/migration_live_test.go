package store

import (
	"os"
	"testing"

	"go.uber.org/zap"
)

// Migrations must run clean against a real database, not only a fresh one.
// Point KARMAX_TEST_DB at a COPY of a live database — never the live one.
func TestMigrationsAgainstARealDatabase(t *testing.T) {
	path := os.Getenv("KARMAX_TEST_DB")
	if path == "" {
		t.Skip("set KARMAX_TEST_DB to a copy of a real database")
	}
	s, err := New(path, zap.NewNop())
	if err != nil {
		t.Fatalf("migrations failed on a real database: %v", err)
	}
	defer s.Close()

	var tables int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='gitloom_outbox'`,
	).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Errorf("gitloom_outbox survived the migration")
	}

	var memories int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_entries`).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	t.Logf("memory_entries preserved: %d", memories)
}
