package store

import (
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

func harnessStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "h.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The row exists before the process does, so a crash in between leaves
// something the startup sweep can find.
func TestASessionIsRememberedAcrossRestarts(t *testing.T) {
	s := harnessStore(t)
	now := time.Now().Truncate(time.Second)

	if err := s.SaveHarnessSession(HarnessSession{
		Key: "chat:x", HarnessSessionID: "uuid-1", Kind: "chat", Model: "sonnet",
		State: HarnessStarting, Workdir: "/tmp/x", StartedAt: now, LastActivityAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetHarnessSession("chat:x")
	if err != nil || got == nil {
		t.Fatalf("session not found: %v", err)
	}
	if got.HarnessSessionID != "uuid-1" {
		t.Errorf("session id = %q — without it the transcript cannot be resumed", got.HarnessSessionID)
	}
}

// Usage accumulates in SQL. Read-modify-write in Go would lose one of two turns
// that finished together, and the lost one would vanish from the bill.
func TestTurnsAccumulate(t *testing.T) {
	s := harnessStore(t)
	now := time.Now()
	_ = s.SaveHarnessSession(HarnessSession{
		Key: "chat:y", HarnessSessionID: "u", State: HarnessLive,
		StartedAt: now, LastActivityAt: now,
	})

	for i := 0; i < 3; i++ {
		if err := s.RecordHarnessTurn("chat:y", 0.02, 100, 10, 12771, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	got, _ := s.GetHarnessSession("chat:y")
	if got.Turns != 3 {
		t.Errorf("turns = %d, want 3", got.Turns)
	}
	if got.CostUSD < 0.059 || got.CostUSD > 0.061 {
		t.Errorf("cost = %f, want ~0.06", got.CostUSD)
	}
	if got.CacheRead != 3*12771 {
		t.Errorf("cache reads = %d, want %d", got.CacheRead, 3*12771)
	}
}

// Reopening a key must keep its identity and its history, not start a new one.
func TestReopeningKeepsTheTranscriptAndTotals(t *testing.T) {
	s := harnessStore(t)
	now := time.Now()
	_ = s.SaveHarnessSession(HarnessSession{
		Key: "chat:z", HarnessSessionID: "uuid-9", State: HarnessLive,
		StartedAt: now, LastActivityAt: now,
	})
	_ = s.RecordHarnessTurn("chat:z", 0.05, 1, 1, 1, now)

	// The supervisor re-saves on every open.
	_ = s.SaveHarnessSession(HarnessSession{
		Key: "chat:z", HarnessSessionID: "uuid-9", State: HarnessLive,
		PID: 4242, StartedAt: now, LastActivityAt: time.Now(),
	})

	got, _ := s.GetHarnessSession("chat:z")
	if got.Turns != 1 || got.CostUSD == 0 {
		t.Errorf("reopening reset the totals: turns=%d cost=%f", got.Turns, got.CostUSD)
	}
	if got.PID != 4242 {
		t.Errorf("pid = %d, want the new process", got.PID)
	}
}

func TestListFiltersByState(t *testing.T) {
	s := harnessStore(t)
	now := time.Now()
	_ = s.SaveHarnessSession(HarnessSession{Key: "a", HarnessSessionID: "1", State: HarnessLive, StartedAt: now, LastActivityAt: now})
	_ = s.SaveHarnessSession(HarnessSession{Key: "b", HarnessSessionID: "2", State: HarnessDead, StartedAt: now, LastActivityAt: now})

	live, err := s.ListHarnessSessions(HarnessLive)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Key != "a" {
		t.Errorf("live = %+v, want just a", live)
	}
	if all, _ := s.ListHarnessSessions(); len(all) != 2 {
		t.Errorf("unfiltered list returned %d, want 2", len(all))
	}
}

func TestAnUnknownKeyIsNotAnError(t *testing.T) {
	s := harnessStore(t)
	got, err := s.GetHarnessSession("never-existed")
	if err != nil {
		t.Errorf("unknown key returned an error: %v", err)
	}
	if got != nil {
		t.Errorf("unknown key returned %+v", got)
	}
}
