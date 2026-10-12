package ledger

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
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
	if err := db.OpenTask("T1", "agent-03", "operator", t0); err != nil {
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

func TestTasksKeepTheirAssigner(t *testing.T) {
	db := open(t)
	if err := db.OpenTask("T9", "agent-03", "mira", t0); err != nil {
		t.Fatal(err)
	}
	task, err := db.Task("T9")
	if err != nil || task.Assigner != "mira" {
		t.Fatalf("task = %+v, %v", task, err)
	}
}

// A ledger made before tasks had an assigner gains the column on open.
func TestOpenAddsTheAssignerColumnToAnOldLedger(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE tasks (id TEXT PRIMARY KEY, agent TEXT, assigned INTEGER, closed INTEGER, outcome TEXT, branch TEXT, pr TEXT)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.OpenTask("T1", "agent-03", "operator", t0); err != nil {
		t.Fatal(err)
	}
}

func TestInboxIsTheOperatorsMessagesAfterAnID(t *testing.T) {
	db := open(t)
	for _, r := range []Relay{{From: "agent-03", To: "operator", Text: "a"}, {From: "operator", To: "agent-03", Text: "b"}, {From: "agent-05", To: "operator", Text: "c"}} {
		r.At, r.Delivered = t0, true
		if err := db.AddRelay(r); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := db.Inbox(0)
	if len(all) != 2 || all[0].Text != "a" || all[1].Text != "c" {
		t.Fatalf("inbox = %+v", all)
	}
	if rest, _ := db.Inbox(all[0].ID); len(rest) != 1 || rest[0].Text != "c" {
		t.Fatalf("after %d = %+v", all[0].ID, rest)
	}
}

func TestEventsAfterIsACursorAndFiltersKinds(t *testing.T) {
	db := open(t)
	// two events in the same second, then one later: the cursor must not skip or repeat
	for _, k := range []string{"Stop", "fleet.done", "fleet.rotate", "PreToolUse"} {
		_ = db.AddEvent(Event{Agent: "agent-01", At: t0, Kind: k, Detail: `{"x":1}`})
	}
	p1, err := db.EventsWhere(EventQuery{Limit: 2})
	if err != nil || len(p1) != 2 {
		t.Fatalf("page 1 = %v, %v", p1, err)
	}
	p2, _ := db.EventsWhere(EventQuery{After: p1[1].ID, Limit: 2})
	if len(p2) != 2 || p2[0].ID != p1[1].ID+1 {
		t.Fatalf("page 2 = %+v after %d", p2, p1[1].ID)
	}
	if p3, _ := db.EventsWhere(EventQuery{After: p2[1].ID}); len(p3) != 0 {
		t.Fatalf("page 3 = %+v, want none", p3)
	}
	got, _ := db.EventsWhere(EventQuery{Kinds: []string{"Stop", "fleet."}})
	if len(got) != 3 {
		t.Fatalf("kinds Stop + fleet.* = %d events, want 3", len(got))
	}
	if odd, _ := db.EventsWhere(EventQuery{Kinds: []string{"fleet_"}}); len(odd) != 0 {
		t.Fatal("an underscore in a prefix must not be a wildcard")
	}
	newest, _ := db.EventsWhere(EventQuery{Limit: 2, Newest: true})
	if len(newest) != 2 || newest[0].Kind != "fleet.rotate" || newest[1].Kind != "PreToolUse" {
		t.Fatalf("newest 2 = %+v, want the last two, oldest first", newest)
	}
}

func TestEventJSONCarriesIDAndRawDetail(t *testing.T) {
	b, _ := json.Marshal(Event{ID: 5, Agent: "a", Kind: "Stop", Detail: `{"tool":"Bash"}`})
	if !strings.Contains(string(b), `"id":5`) || !strings.Contains(string(b), `"detail":{"tool":"Bash"}`) {
		t.Fatalf("event JSON = %s", b)
	}
	var back Event
	if err := json.Unmarshal(b, &back); err != nil || back.Detail != `{"tool":"Bash"}` || back.ID != 5 {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	b, _ = json.Marshal(Event{Kind: "x", Detail: "not json"})
	if !strings.Contains(string(b), `"detail":"not json"`) {
		t.Fatalf("non-JSON detail = %s", b)
	}
}

func TestActivityCountsBuckets(t *testing.T) {
	db := open(t)
	for _, d := range []time.Duration{0, time.Minute, 16 * time.Minute, 31 * time.Minute, 32 * time.Minute, 33 * time.Minute} {
		_ = db.AddEvent(Event{Agent: "agent-01", At: t0.Add(d), Kind: "Stop"})
	}
	_ = db.AddEvent(Event{Agent: "agent-02", At: t0, Kind: "Stop"})
	got, err := db.Activity(t0.Add(-time.Hour), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	a := got["agent-01"]
	if len(a) != 3 || a[0].N != 2 || a[1].N != 1 || a[2].N != 3 || !a[0].T.Equal(t0) {
		t.Fatalf("agent-01 buckets = %+v", a)
	}
	if len(got["agent-02"]) != 1 {
		t.Fatalf("agent-02 = %+v", got["agent-02"])
	}
}

func TestTasksOpenAndNewest(t *testing.T) {
	db := open(t)
	_ = db.OpenTask("T1", "agent-01", "operator", t0)
	_ = db.OpenTask("T2", "agent-01", "mira", t0.Add(time.Hour))
	_ = db.OpenTask("T3", "agent-02", "operator", t0.Add(2*time.Hour))
	_ = db.CloseTask("T1", t0.Add(30*time.Minute), "done", "", "")
	open, _ := db.Tasks(true, "", 0)
	if len(open) != 2 || open[0].ID != "T3" || open[1].ID != "T2" {
		t.Fatalf("open = %+v", open)
	}
	last, _ := db.Tasks(false, "agent-01", 1)
	if len(last) != 1 || last[0].ID != "T2" {
		t.Fatalf("newest of agent-01 = %+v", last)
	}
	tot, _ := db.Totals(t0)
	if tot.TasksClosed != 1 {
		t.Fatalf("totals = %+v", tot)
	}
}

func TestSessionJSONIsSnakeCase(t *testing.T) {
	b, _ := json.Marshal(Session{ID: "s", Agent: "a", Task: "T", CostUSD: 1.5, CacheRead: 3})
	for _, want := range []string{`"task":"T"`, `"cost_usd":1.5`, `"cache_read":3`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("session JSON %s lacks %s", b, want)
		}
	}
}

// A ledger made before reports belonged to tasks gains relays.task on open.
func TestOpenAddsTheRelayTaskColumnToAnOldLedger(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE relays (id INTEGER PRIMARY KEY AUTOINCREMENT, sender TEXT, recipient TEXT, at INTEGER, size INTEGER, delivered INTEGER, text TEXT, error TEXT)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.AddRelay(Relay{From: "agent-01", To: "operator", Task: "T1", Text: "x", At: t0}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskReportsAreThoseOfOneTaskAfterAnIDOldestFirst(t *testing.T) {
	db := open(t)
	for _, r := range []Relay{
		{From: "agent-01", To: "operator", Task: "T1", Text: "a"},
		{From: "agent-02", To: "operator", Task: "T2", Text: "other task"},
		{From: "agent-01", To: "karmax", Task: "T1", Text: "b"},
		{From: "agent-01", To: "operator", Text: "no task"},
		{From: "agent-01", To: "operator", Task: "T1", Text: "c"},
	} {
		r.At, r.Delivered = t0, true
		if err := db.AddRelay(r); err != nil {
			t.Fatal(err)
		}
	}
	all, err := db.TaskReports("T1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Text != "a" || all[1].Text != "b" || all[2].Text != "c" {
		t.Fatalf("reports = %+v", all)
	}
	if all[1].To != "karmax" || all[1].Task != "T1" || all[1].From != "agent-01" {
		t.Errorf("relay = %+v", all[1])
	}
	later, _ := db.TaskReports("T1", all[0].ID)
	if len(later) != 2 || later[0].Text != "b" {
		t.Fatalf("after = %+v", later)
	}
	if none, _ := db.TaskReports("T9", 0); len(none) != 0 {
		t.Fatalf("T9 = %+v", none)
	}
}

func TestInboxCarriesTheTask(t *testing.T) {
	db := open(t)
	_ = db.AddRelay(Relay{From: "agent-01", To: "operator", Task: "T1", Text: "a", At: t0})
	got, _ := db.Inbox(0)
	if len(got) != 1 || got[0].Task != "T1" {
		t.Fatalf("inbox = %+v", got)
	}
}

func TestTaskKeepsItsRepo(t *testing.T) {
	db := open(t)
	_ = db.OpenTask("T1", "agent-01", "operator", t0)
	if err := db.SetTaskRepo("T1", "app"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.Task("T1"); got.Repo != "app" {
		t.Fatalf("task = %+v", got)
	}
	if all, _ := db.Tasks(false, "", 0); len(all) != 1 || all[0].Repo != "app" {
		t.Fatalf("tasks = %+v", all)
	}
}

func TestTaskRepoAndLastRepo(t *testing.T) {
	db := open(t)
	if r, err := db.LastRepo("agent-01"); err != nil || r != "" {
		t.Fatalf("LastRepo with no tasks = %q, %v", r, err)
	}
	for i, id := range []string{"T1", "T2"} {
		if err := db.OpenTask(id, "agent-01", "operator", t0.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetTaskRepo("T1", "KARMAX"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTaskRepo("T2", "site"); err != nil {
		t.Fatal(err)
	}
	if r, _ := db.LastRepo("agent-01"); r != "site" {
		t.Errorf("LastRepo = %q, want the newest task's repo", r)
	}
	if r, _ := db.LastRepo("agent-02"); r != "" {
		t.Errorf("another agent's LastRepo = %q", r)
	}
	tk, err := db.Task("T1")
	if err != nil || tk.Repo != "KARMAX" {
		t.Fatalf("Task = %+v, %v", tk, err)
	}
	all, _ := db.Tasks(false, "agent-01", 0)
	if len(all) != 2 || all[0].Repo != "site" || all[1].Repo != "KARMAX" {
		t.Errorf("Tasks = %+v", all)
	}
}

func TestMigrationAddsTasksRepo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE tasks (id TEXT PRIMARY KEY, agent TEXT, assigned INTEGER, closed INTEGER,
		outcome TEXT, branch TEXT, pr TEXT, assigner TEXT); INSERT INTO tasks(id, agent, assigned) VALUES('T0','agent-01',1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetTaskRepo("T0", "r"); err != nil {
		t.Fatal(err)
	}
	if tk, _ := db.Task("T0"); tk.Repo != "r" {
		t.Errorf("Task = %+v", tk)
	}
}
