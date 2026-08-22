package scheduler

import (
	"path/filepath"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func testScheduler(t *testing.T) *Scheduler {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "s.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, nil, zap.NewNop())
}

// One request, restated across three chats under three spellings, became six
// jobs and a triple-fired nudge. Re-adding is refreshing, not breeding.
func TestReAddingAJobByNameReplacesIt(t *testing.T) {
	s := testScheduler(t)
	if err := s.AddJob(ScheduledJob{Name: "leetcode_morning_nudge", Cron: "0 0 8 * * *", AgentID: "nexus", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddJob(ScheduledJob{Name: "leetcode-morning-nudge", Cron: "0 0 9 * * *", AgentID: "nexus", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	jobs := s.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1 — the re-add must replace, not duplicate", len(jobs))
	}
	if jobs[0].Cron != "0 0 9 * * *" {
		t.Errorf("the refreshed job should carry the new schedule, got %q", jobs[0].Cron)
	}
}

// Different agents may own a job of the same name.
func TestSameNameDifferentAgentIsADifferentJob(t *testing.T) {
	s := testScheduler(t)
	_ = s.AddJob(ScheduledJob{Name: "daily", Cron: "0 0 8 * * *", AgentID: "nexus", Enabled: true})
	_ = s.AddJob(ScheduledJob{Name: "daily", Cron: "0 0 8 * * *", AgentID: "other", Enabled: true})
	if got := len(s.ListJobs()); got != 2 {
		t.Fatalf("got %d jobs, want 2", got)
	}
}
