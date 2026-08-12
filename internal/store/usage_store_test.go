package store

import (
	"go.uber.org/zap"
	"path/filepath"
	"testing"
	"time"
)

func usageTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "test.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUsageIsRecordedAndTotalled(t *testing.T) {
	s := usageTestStore(t)
	for _, u := range []ModelUsage{
		{AgentID: "nexus", Model: "sonnet", Kind: "main", InputTokens: 1000, OutputTokens: 100},
		{AgentID: "nexus", Model: "sonnet", Kind: "main", InputTokens: 500, OutputTokens: 50},
		{AgentID: "nexus", Model: "haiku", Kind: "summary", InputTokens: 200, OutputTokens: 20},
	} {
		if err := s.RecordModelUsage(u); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	totals, err := s.UsageSince(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("UsageSince: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("expected one row per model+kind, got %d: %+v", len(totals), totals)
	}
	// Heaviest first.
	if totals[0].Model != "sonnet" || totals[0].Calls != 2 || totals[0].InputTokens != 1500 {
		t.Errorf("sonnet total wrong: %+v", totals[0])
	}
	if totals[1].Model != "haiku" || totals[1].Kind != "summary" {
		t.Errorf("haiku total wrong: %+v", totals[1])
	}
}

// A provider that reports nothing must look like missing data, not free
// inference — this is exactly what the old gateway did on every single call.
func TestZeroUsageIsNotStored(t *testing.T) {
	s := usageTestStore(t)
	if err := s.RecordModelUsage(ModelUsage{AgentID: "nexus", Model: "sonnet"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	totals, err := s.UsageSince(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("UsageSince: %v", err)
	}
	if len(totals) != 0 {
		t.Errorf("a zero-token call was stored: %+v", totals)
	}
}

func TestUsageWindowExcludesOlderRows(t *testing.T) {
	s := usageTestStore(t)
	old := ModelUsage{AgentID: "n", Model: "m", InputTokens: 1, CreatedAt: time.Now().Add(-48 * time.Hour)}
	if err := s.RecordModelUsage(old); err != nil {
		t.Fatalf("record: %v", err)
	}
	totals, err := s.UsageSince(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("UsageSince: %v", err)
	}
	if len(totals) != 0 {
		t.Errorf("a row outside the window was counted: %+v", totals)
	}
}
