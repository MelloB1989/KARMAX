package ledger

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestStateRoundTrips(t *testing.T) {
	db := open(t)
	st := reconcile.State{Name: "agent-03", Phase: reconcile.Waiting, Task: "T1", SessionID: "s1",
		Quota: reconcile.Quota{FiveHour: 82, At: t0}, Restarts: []time.Time{t0}}
	if err := db.SaveState(st); err != nil {
		t.Fatal(err)
	}
	all, err := db.States()
	if err != nil {
		t.Fatal(err)
	}
	got := all["agent-03"]
	if got.Phase != reconcile.Waiting || got.Task != "T1" || got.Quota.FiveHour != 82 || len(got.Restarts) != 1 {
		t.Fatalf("state = %+v", got)
	}
}

// Reopening the same file keeps everything: the ledger is the fleet's system
// of record, and fleetd restarts.
func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SaveState(reconcile.State{Name: "agent-01", Phase: reconcile.Standby})
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if all, _ := db.States(); all["agent-01"].Phase != reconcile.Standby {
		t.Fatal("state lost across reopen")
	}
}

func TestUsageSumsPerAgentModelHour(t *testing.T) {
	db := open(t)
	for _, u := range []Usage{
		{Agent: "agent-01", Model: "sonnet", At: t0.Add(5 * time.Minute), Input: 10, Output: 5, CostUSD: 0.1},
		{Agent: "agent-01", Model: "sonnet", At: t0.Add(40 * time.Minute), Input: 1, Output: 1, CostUSD: 0.05},
		{Agent: "agent-01", Model: "opus", At: t0.Add(10 * time.Minute), Output: 100, CostUSD: 1},
		{Agent: "agent-02", Model: "sonnet", At: t0.Add(2 * time.Hour), Input: 7},
	} {
		if err := db.AddUsage(u); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.UsageBy("agent", t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Key != "agent-01" || rows[0].Input != 11 || rows[0].Output != 106 || rows[0].CostUSD < 1.149 {
		t.Fatalf("by agent = %+v", rows)
	}
	byModel, _ := db.UsageBy("model", t0.Add(-time.Hour))
	if len(byModel) != 2 || byModel[0].Key != "opus" && byModel[1].Key != "opus" {
		t.Fatalf("by model = %+v", byModel)
	}
}

func TestEventsQuery(t *testing.T) {
	db := open(t)
	_ = db.AddEvent(Event{Agent: "agent-03", At: t0.Add(-3 * time.Hour), Kind: "Stop"})
	_ = db.AddEvent(Event{Agent: "agent-03", At: t0.Add(-time.Hour), Kind: "fleet.rotate", Detail: `{"reason":"task_done"}`})
	_ = db.AddEvent(Event{Agent: "agent-04", At: t0, Kind: "Stop"})
	evs, err := db.Events("agent-03", t0.Add(-2*time.Hour))
	if err != nil || len(evs) != 1 || evs[0].Kind != "fleet.rotate" {
		t.Fatalf("events = %+v, %v", evs, err)
	}
}

func TestQuotaKeepsTheLatestPerAccount(t *testing.T) {
	db := open(t)
	_ = db.AddQuota(Quota{Account: "acct-1", At: t0, FiveHour: 10, Source: "statusline"})
	_ = db.AddQuota(Quota{Account: "acct-1", At: t0.Add(time.Minute), FiveHour: 12, Source: "statusline"})
	_ = db.AddQuota(Quota{Account: "acct-2", At: t0, SevenDay: 50, Source: "stopfailure"})
	q, err := db.LatestQuota()
	if err != nil || len(q) != 2 || q["acct-1"].FiveHour != 12 || q["acct-2"].SevenDay != 50 {
		t.Fatalf("latest = %+v, %v", q, err)
	}
}

func TestSessionsAndTasks(t *testing.T) {
	db := open(t)
	if err := db.OpenTask("T1", "agent-03", t0); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseTask("T1", t0.Add(time.Hour), "done", "fleet/agent-03/fix", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession(Session{ID: "s1", Agent: "agent-03", Task: "T1", Started: t0, Ended: t0.Add(time.Hour),
		Reason: "task_done", Turns: 4, Output: 900, CostUSD: 0.7, ArchiveID: "agent-03/2026-10-08-s1"}); err != nil {
		t.Fatal(err)
	}
	hist, err := db.History("agent-03", 10)
	if err != nil || len(hist) != 1 || hist[0].ArchiveID == "" || hist[0].Turns != 4 {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	task, err := db.Task("T1")
	if err != nil || task.Outcome != "done" || task.Branch != "fleet/agent-03/fix" || task.Closed.IsZero() {
		t.Fatalf("task = %+v, %v", task, err)
	}
}

func TestRetention(t *testing.T) {
	db := open(t)
	now := t0
	_ = db.AddEvent(Event{Agent: "a", At: now.Add(-100 * 24 * time.Hour), Kind: "Stop"})
	_ = db.AddEvent(Event{Agent: "a", At: now.Add(-100*24*time.Hour + time.Hour), Kind: "Stop"})
	_ = db.AddEvent(Event{Agent: "a", At: now.Add(-time.Hour), Kind: "Stop"})
	// Ten samples in one hour, eight days ago: one survives.
	for i := 0; i < 10; i++ {
		_ = db.AddQuota(Quota{Account: "acct", At: now.Add(-8*24*time.Hour + time.Duration(i)*time.Minute), FiveHour: float64(i)})
	}
	_ = db.AddRelay(Relay{From: "agent-01", To: "agent-05", At: now.Add(-40 * 24 * time.Hour), Text: "old", Delivered: true})
	_ = db.AddRelay(Relay{From: "agent-01", To: "agent-05", At: now.Add(-time.Hour), Text: "new", Delivered: true})
	_ = db.AddHostSample(HostSample{Container: "agent-01", At: now.Add(-8 * 24 * time.Hour), CPU: 1})

	if err := db.Retain(now, Retention{EventsRaw: 90 * 24 * time.Hour, QuotaRaw: 7 * 24 * time.Hour,
		Relays: 30 * 24 * time.Hour, HostSamples: 7 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if evs, _ := db.Events("a", time.Time{}); len(evs) != 1 {
		t.Errorf("raw events left = %d, want 1", len(evs))
	}
	if daily, _ := db.DailyEventCounts("a"); len(daily) != 1 || daily[0].Count != 2 {
		t.Errorf("daily counts = %+v", daily)
	}
	if n := db.count(t, "SELECT COUNT(*) FROM quota"); n != 1 {
		t.Errorf("quota samples left = %d, want 1 per hour", n)
	}
	if n := db.count(t, "SELECT COUNT(*) FROM relays"); n != 1 {
		t.Errorf("relays left = %d", n)
	}
	if n := db.count(t, "SELECT COUNT(*) FROM host"); n != 0 {
		t.Errorf("host samples left = %d", n)
	}
}

func (db *DB) count(t *testing.T, q string) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
