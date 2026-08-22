package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
)

// The rest of this package tests translation by construction: one dialect per
// file, each checked against SQLite's output. This file instead points the
// real drivers at real servers and runs the same assertions on all three, so a
// translation that is well-formed but wrong — a WHERE clause that changed
// meaning, a timestamp that changed zone — fails here even though it would
// pass a unit test of the rewriter alone.
//
// Point KARMAX_TEST_POSTGRES_URL / KARMAX_TEST_MYSQL_URL at THROWAWAY servers.
// Each subtest creates and drops its own schema, but never point these at
// anything that matters.

func TestLiveBackends(t *testing.T) {
	backends := []struct {
		name string
		open func(t *testing.T) (dsn string, cleanup func())
	}{
		{"sqlite", sqliteLiveDSN},
		{"postgres", postgresLiveDSN},
		{"mysql", mysqlLiveDSN},
	}

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			dsn, cleanup := b.open(t)
			defer cleanup()

			s, err := New(dsn, zap.NewNop())
			if err != nil {
				t.Fatalf("open store (runs every migration): %v", err)
			}
			defer s.Close()

			t.Run("memory", func(t *testing.T) { liveMemoryEntries(t, s) })
			t.Run("forgetOrdering", func(t *testing.T) { liveForgetOrdering(t, s) })
			t.Run("deleteOldMemoryEntries", func(t *testing.T) { liveDeleteOldMemoryEntries(t, s) })
			t.Run("searchCaseInsensitive", func(t *testing.T) { liveSearchCaseInsensitive(t, s) })
			t.Run("pageIndexTree", func(t *testing.T) { livePageIndexTree(t, s) })
			t.Run("kv", func(t *testing.T) { liveKV(t, s) })
			t.Run("eventLog", func(t *testing.T) { liveEventLog(t, s) })
			t.Run("usageByDay", func(t *testing.T) { liveUsageByDay(t, s) })
			t.Run("agentTurn", func(t *testing.T) { liveStartAgentTurn(t, s) })
			t.Run("loopLease", func(t *testing.T) { liveLoopLease(t, s) })
			t.Run("loopRuns", func(t *testing.T) { liveLoopRuns(t, s) })
			t.Run("loopSteps", func(t *testing.T) { liveLoopSteps(t, s) })
			t.Run("timers", func(t *testing.T) { liveTimers(t, s) })
			t.Run("timestampRoundTrip", func(t *testing.T) { liveTimestampRoundTrip(t, s) })
		})
	}
}

// sqliteLiveDSN gives every run its own file, same as newTestStore.
func sqliteLiveDSN(t *testing.T) (string, func()) {
	return filepath.Join(t.TempDir(), "live.db"), func() {}
}

// postgresLiveDSN creates a throwaway database on the target server and points
// the DSN at it, so a run never touches another run's tables or a previous
// run's leftovers.
func postgresLiveDSN(t *testing.T) (string, func()) {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("KARMAX_TEST_POSTGRES_URL"))
	if base == "" {
		t.Skip("set KARMAX_TEST_POSTGRES_URL to a throwaway postgres server, e.g. postgres://postgres:pw@127.0.0.1:55432/karmax?sslmode=disable")
	}

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("postgres admin connect: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("postgres admin ping: %v", err)
	}

	name := fmt.Sprintf("karmax_live_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", base, err)
	}
	u.Path = "/" + name

	return u.String(), func() {
		admin, err := sql.Open("pgx", base)
		if err != nil {
			t.Logf("postgres cleanup connect: %v", err)
			return
		}
		defer admin.Close()
		if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
			t.Logf("postgres cleanup: drop %s: %v", name, err)
		}
	}
}

// mysqlLiveDSN mirrors postgresLiveDSN: a throwaway database per run. Reuses
// the package's own mysqlFromURL so the admin connection is parsed exactly the
// way New() would parse it.
func mysqlLiveDSN(t *testing.T) (string, func()) {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("KARMAX_TEST_MYSQL_URL"))
	if base == "" {
		t.Skip("set KARMAX_TEST_MYSQL_URL to a throwaway mysql server, e.g. mysql://root:pw@127.0.0.1:53306/karmax")
	}

	adminDSN, err := mysqlFromURL(base)
	if err != nil {
		t.Fatalf("parse %s: %v", base, err)
	}
	admin, err := sql.Open("mysql", adminDSN)
	if err != nil {
		t.Fatalf("mysql admin connect: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("mysql admin ping: %v", err)
	}

	name := fmt.Sprintf("karmax_live_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}

	cfg, err := mysql.ParseDSN(adminDSN)
	if err != nil {
		t.Fatalf("parse dsn %s: %v", adminDSN, err)
	}
	cfg.DBName = name

	return cfg.FormatDSN(), func() {
		admin, err := sql.Open("mysql", adminDSN)
		if err != nil {
			t.Logf("mysql cleanup connect: %v", err)
			return
		}
		defer admin.Close()
		if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + name + "`"); err != nil {
			t.Logf("mysql cleanup: drop %s: %v", name, err)
		}
	}
}

const liveNS = "live-ns"

// liveMemoryEntries exercises placeholders, LIKE, LIMIT, the boolean-as-int
// pinned column, the forgetting subquery+LIMIT, expiry comparison, and the
// datetime('now') touch path.
func liveMemoryEntries(t *testing.T, s *Store) {
	entries := []StoredMemoryEntry{
		{ID: "e1", AgentID: "a", Namespace: liveNS, Role: "user", Content: "critical fact about widgets", Importance: 4},
		{ID: "e2", AgentID: "a", Namespace: liveNS, Role: "user", Content: "low value scratch note", Importance: 1},
		{ID: "e3", AgentID: "a", Namespace: liveNS, Role: "user", Content: "pinned low importance memory", Importance: 1, Pinned: true},
		{ID: "e4", AgentID: "a", Namespace: liveNS, Role: "user", Content: "medium memory about widgets", Tags: "widget,ops", Importance: 2},
	}
	for _, e := range entries {
		if err := s.InsertMemoryEntry(e); err != nil {
			t.Fatalf("insert %s: %v", e.ID, err)
		}
	}

	if n, err := s.CountMemoryEntries(liveNS); err != nil || n != 4 {
		t.Fatalf("count = %d, err = %v, want 4", n, err)
	}

	found, err := s.SearchMemoryEntries(liveNS, "widget", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 2 {
		t.Errorf("search widget matched %d entries, want 2 (content LIKE + tags LIKE)", len(found))
	}

	if err := s.TouchMemoryEntries([]string{"e1"}); err != nil {
		t.Fatalf("touch: %v", err)
	}
	list, err := s.ListMemoryEntries(liveNS, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]StoredMemoryEntry{}
	for _, e := range list {
		byID[e.ID] = e
	}
	e1 := byID["e1"]
	if e1.AccessCount != 1 || e1.LastAccessedAt == nil {
		t.Errorf("touch did not update e1: %+v", e1)
	} else if d := time.Since(*e1.LastAccessedAt); d < -2*time.Minute || d > 2*time.Minute {
		t.Errorf("last_accessed_at = %v, too far from now (datetime('now') translation off by %v)", e1.LastAccessedAt, d)
	}
	if !byID["e3"].Pinned {
		t.Errorf("e3 pinned flag lost in round trip: %+v", byID["e3"])
	}

	// ForgetLeastValuable must pick the lowest importance/access/recency AMONG
	// non-pinned rows: e2 (importance 1, unpinned), never e3 (importance 1 but
	// pinned). Non-fatal: a broken translation here must not hide whatever the
	// rest of this function would otherwise have caught.
	n, err := s.ForgetLeastValuable(liveNS, 1)
	if err != nil || n != 1 {
		t.Errorf("forget = %d, err = %v, want 1", n, err)
	}
	list, _ = s.ListMemoryEntries(liveNS, 10)
	byID = map[string]StoredMemoryEntry{}
	for _, e := range list {
		byID[e.ID] = e
	}
	if _, stillThere := byID["e2"]; stillThere && err == nil {
		t.Error("ForgetLeastValuable reported success but did not remove e2")
	}
	if _, ok := byID["e3"]; !ok {
		t.Error("ForgetLeastValuable removed a pinned entry")
	}

	// Expiry: one already-expired, one not. Only the expired one is pruned.
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	if err := s.InsertMemoryEntry(StoredMemoryEntry{ID: "e5", AgentID: "a", Namespace: liveNS, Role: "user", Content: "expired", ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertMemoryEntry(StoredMemoryEntry{ID: "e6", AgentID: "a", Namespace: liveNS, Role: "user", Content: "not expired", ExpiresAt: &future}); err != nil {
		t.Fatal(err)
	}
	pruned, err := s.PruneExpiredMemories(liveNS)
	if err != nil || pruned != 1 {
		t.Fatalf("prune = %d, err = %v, want 1", pruned, err)
	}
	list, _ = s.ListMemoryEntries(liveNS, 10)
	byID = map[string]StoredMemoryEntry{}
	for _, e := range list {
		byID[e.ID] = e
	}
	if _, gone := byID["e5"]; gone {
		t.Error("the expired entry survived PruneExpiredMemories")
	}
	if _, ok := byID["e6"]; !ok {
		t.Error("PruneExpiredMemories removed an entry that had not expired yet")
	}

	if err := s.UpdateMemoryImportance("e1", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMemoryPinned("e1", true); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMemoryEntry("e1", "revised content"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListMemoryEntries(liveNS, 10)
	for _, e := range list {
		if e.ID == "e1" {
			if e.Importance != 3 || !e.Pinned || e.Content != "revised content" {
				t.Errorf("e1 after updates = %+v", e)
			}
		}
	}

	if err := s.DeleteMemoryEntry("e1"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.KVGet("nope", "nope"); found {
		t.Error("unrelated KV lookup found something")
	}
}

// livePageIndexTree exercises the upsert path — ON CONFLICT DO UPDATE ...
// excluded.x on Postgres/SQLite, ON DUPLICATE KEY UPDATE ... VALUES(x) on
// MySQL — by saving the same namespace twice with different content.
func livePageIndexTree(t *testing.T, s *Store) {
	if err := s.SavePageIndexTree(liveNS, `{"v":1}`, `{"toc":1}`); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if err := s.SavePageIndexTree(liveNS, `{"v":2}`, `{"toc":2}`); err != nil {
		t.Fatalf("save 2 (upsert): %v", err)
	}
	tree, toc, err := s.LoadPageIndexTree(liveNS)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if tree != `{"v":2}` || toc != `{"toc":2}` {
		t.Errorf("tree=%q toc=%q, want the SECOND save's content (upsert did not overwrite)", tree, toc)
	}
}

// liveKV exercises the kv_memory table, whose `key` column is a MySQL
// reserved word, plus TTL expiry filtering on reads and on the sweep.
func liveKV(t *testing.T, s *Store) {
	const grp = "live-grp"
	if err := s.KVSet(grp, "topic", "widgets", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	// KVSet only arms expiry for ttl > 0, so a short TTL plus a sleep is what
	// actually lands expires_at in the past — a negative TTL is silently "no
	// expiry" per the ttl > 0 guard. expires_at is written pre-formatted to
	// whole-second text (no fractional component), so the margin has to clear
	// a full second, not just the TTL, to be safe against landing right on a
	// second boundary.
	if err := s.KVSet(grp, "stale", "gone", 250*time.Millisecond); err != nil {
		t.Fatalf("set expired: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)

	v, found, err := s.KVGet(grp, "topic")
	if err != nil || !found || v != "widgets" {
		t.Fatalf("get topic = %q, found=%v, err=%v", v, found, err)
	}
	if _, found, err := s.KVGet(grp, "stale"); err != nil || found {
		t.Fatalf("get stale: found=%v, err=%v, want found=false", found, err)
	}

	list, err := s.KVList(grp)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Key != "topic" {
		t.Errorf("list = %+v, want only the live entry", list)
	}

	n, err := s.KVPurgeExpired()
	if err != nil || n < 1 {
		t.Fatalf("purge = %d, err = %v, want at least 1", n, err)
	}

	if err := s.KVDelete(grp, "topic"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.KVGet(grp, "topic"); found {
		t.Error("KVDelete left the key readable")
	}

	if err := s.KVSet(grp, "a", "1", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.KVSet(grp, "b", "2", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.KVClearGroup(grp); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.KVList(grp); len(list) != 0 {
		t.Errorf("KVClearGroup left %d entries", len(list))
	}
}

// liveEventLog exercises the auto-increment seq, the ON CONFLICT(event_id) DO
// NOTHING dedup path, and the offset upsert — whose SET clause uses SQLite's
// two-argument MAX, translated to GREATEST on Postgres and to a MySQL
// ON DUPLICATE KEY UPDATE with GREATEST(col, VALUES(col)).
func liveEventLog(t *testing.T, s *Store) {
	seq1, err := s.AppendLogEvent(LogEvent{ID: "ev-1", Kind: "test.created", Payload: map[string]any{"n": float64(1)}})
	if err != nil {
		t.Fatalf("append 1: %v", err)
	}
	seq1b, err := s.AppendLogEvent(LogEvent{ID: "ev-1", Kind: "test.created", Payload: map[string]any{"n": float64(999)}})
	if err != nil {
		t.Fatalf("append duplicate: %v", err)
	}
	if seq1b != seq1 {
		t.Errorf("re-appending event id ev-1 got seq %d, want the original %d", seq1b, seq1)
	}
	// The dedup path (insertReturningID: RETURNING on Postgres, LastInsertId
	// elsewhere) must not leave a second row behind even when it correctly
	// reports the original seq.
	var evRows int
	if err := s.queryRow(`SELECT COUNT(*) FROM event_log WHERE event_id = ?`, "ev-1").Scan(&evRows); err != nil {
		t.Fatalf("count ev-1 rows: %v", err)
	}
	if evRows != 1 {
		t.Errorf("event_log has %d rows for event_id ev-1 after a duplicate append, want exactly 1", evRows)
	}

	seq2, err := s.AppendLogEvent(LogEvent{ID: "ev-2", Kind: "test.updated"})
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if seq2 <= seq1 {
		t.Errorf("seq2 = %d, want greater than seq1 = %d", seq2, seq1)
	}

	events, err := s.LogEventsAfter(DefaultWorkspace, 0, nil, 10)
	if err != nil {
		t.Fatalf("events after: %v", err)
	}
	if len(events) != 2 || events[0].ID != "ev-1" || events[1].ID != "ev-2" {
		t.Fatalf("events = %+v, want [ev-1 ev-2] in seq order", events)
	}
	if n, _ := events[0].Payload["n"].(float64); n != 1 {
		t.Errorf("ev-1 payload = %v, want the FIRST insert's payload (dedup must not overwrite)", events[0].Payload)
	}

	head, err := s.LogHead(DefaultWorkspace)
	if err != nil || head != seq2 {
		t.Errorf("head = %d, err = %v, want %d", head, err, seq2)
	}

	if _, known, err := s.ConsumerOffset("sub-1", DefaultWorkspace); err != nil || known {
		t.Fatalf("fresh offset: known=%v, err=%v, want known=false", known, err)
	}
	if err := s.SetConsumerOffset("sub-1", DefaultWorkspace, 5); err != nil {
		t.Fatal(err)
	}
	if off, known, err := s.ConsumerOffset("sub-1", DefaultWorkspace); err != nil || !known || off != 5 {
		t.Fatalf("offset = %d, known=%v, err=%v, want 5/true", off, known, err)
	}
	// A lower offset must not move the stored one backwards — this is the
	// two-arg MAX/GREATEST translation under test.
	if err := s.SetConsumerOffset("sub-1", DefaultWorkspace, 3); err != nil {
		t.Fatal(err)
	}
	if off, _, err := s.ConsumerOffset("sub-1", DefaultWorkspace); err != nil || off != 5 {
		t.Errorf("offset after a lower SetConsumerOffset = %d, want unchanged 5 (MAX/GREATEST translation)", off)
	}
	// A higher offset does move it.
	if err := s.SetConsumerOffset("sub-1", DefaultWorkspace, 9); err != nil {
		t.Fatal(err)
	}
	if off, _, err := s.ConsumerOffset("sub-1", DefaultWorkspace); err != nil || off != 9 {
		t.Errorf("offset after a higher SetConsumerOffset = %d, want 9", off)
	}

	if safe, err := s.SafeSeq(DefaultWorkspace); err != nil || safe != 9 {
		t.Errorf("safe seq = %d, err = %v, want 9", safe, err)
	}

	if err := s.RecordDeadLetter(DeadLetter{Subscriber: "sub-1", EventSeq: seq1, EventID: "ev-1", Kind: "test.created", Attempts: 3, LastError: "boom"}); err != nil {
		t.Fatal(err)
	}
	dl, err := s.DeadLetters(10)
	if err != nil || len(dl) != 1 || dl[0].LastError != "boom" {
		t.Errorf("dead letters = %+v, err = %v", dl, err)
	}
}

// liveLoopLease exercises AcquireLoopLease's compare-and-set upsert, which has
// no direct MySQL translation and carries a MySQL-specific form (see
// leaseSQL in loop_run_store.go) — this is the highest-risk statement in the
// package for a subtle per-backend divergence.
func liveLoopLease(t *testing.T, s *Store) {
	const loop = "lease-loop"

	got, err := s.AcquireLoopLease(loop, "owner-1", time.Minute)
	if err != nil || !got {
		t.Fatalf("first acquire: got=%v err=%v", got, err)
	}
	if got, err := s.AcquireLoopLease(loop, "owner-2", time.Minute); err != nil || got {
		t.Fatalf("second acquire while held: got=%v err=%v, want false", got, err)
	}
	if err := s.ReleaseLoopLease(loop, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AcquireLoopLease(loop, "owner-3", time.Minute); err != nil || !got {
		t.Fatalf("acquire after release: got=%v err=%v, want true", got, err)
	}

	// An expired lease must be reclaimable even though its owner never
	// released it — a crashed process cannot wedge the loop closed.
	if got, err := s.AcquireLoopLease("stuck-loop", "dead", -time.Minute); err != nil || !got {
		t.Fatalf("acquire with negative ttl: got=%v err=%v", got, err)
	}
	if got, err := s.AcquireLoopLease("stuck-loop", "live", time.Minute); err != nil || !got {
		t.Fatalf("reclaim expired lease: got=%v err=%v, want true", got, err)
	}
	// The stale owner must not be able to release the NEW holder's lease.
	if err := s.ReleaseLoopLease("stuck-loop", "dead"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AcquireLoopLease("stuck-loop", "third", time.Minute); err != nil || got {
		t.Fatalf("acquire after stale release: got=%v err=%v, want false (live still holds it)", got, err)
	}

	// Concurrent racers against the real network round trip: exactly one may
	// win, which is only true if the compare-and-set is actually atomic.
	const racers = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := s.AcquireLoopLease("contended-loop", fmt.Sprintf("r%d", i), time.Minute)
			if err == nil && ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d racers won the lease concurrently, want exactly 1", wins)
	}
}

// liveLoopRuns exercises StartLoopRun/FinishLoopRun, including the
// millisBetween duration expression and the retry-standing upsert.
func liveLoopRuns(t *testing.T, s *Store) {
	start := time.Now()
	if err := s.StartLoopRun(LoopRun{ID: "run-1", Loop: "run-loop", Trigger: "schedule", Attempt: 1, StartedAt: start}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.FinishLoopRun("run-1", "run-loop", "ok", "", 1, nil); err != nil {
		t.Fatalf("finish ok: %v", err)
	}

	states, err := s.LoopStates()
	if err != nil {
		t.Fatalf("states: %v", err)
	}
	var st *LoopState
	for i := range states {
		if states[i].Loop == "run-loop" {
			st = &states[i]
		}
	}
	if st == nil || st.LastSuccessAt == nil || st.ConsecFails != 0 {
		t.Fatalf("state after success = %+v", st)
	}

	if err := s.StartLoopRun(LoopRun{ID: "run-2", Loop: "run-loop", Attempt: 2, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	retry := time.Now().Add(time.Minute)
	if err := s.FinishLoopRun("run-2", "run-loop", "failed", "gateway down", 2, &retry); err != nil {
		t.Fatalf("finish failed: %v", err)
	}
	due, err := s.DueLoopRetries(retry.Add(time.Second))
	if err != nil {
		t.Fatalf("due retries: %v", err)
	}
	var foundRetry bool
	for _, d := range due {
		if d.Loop == "run-loop" && d.RetryAttempt == 2 {
			foundRetry = true
		}
	}
	if !foundRetry {
		t.Errorf("due retries = %+v, want run-loop attempt 2", due)
	}

	runs, err := s.RecentLoopRuns("run-loop", 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("recent runs = %+v, err = %v, want 2", runs, err)
	}
	for _, r := range runs {
		if r.ID == "run-1" && r.DurationMS < 0 {
			t.Errorf("run-1 duration_ms = %d, want >= 0 (millisBetween translation)", r.DurationMS)
		}
	}

	if err := s.StartLoopRun(LoopRun{ID: "orphan", Loop: "reap-loop", Attempt: 1, StartedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	reaped, err := s.ReapStaleLoopRuns(time.Now().Add(-30 * time.Minute))
	if err != nil || len(reaped) != 1 || reaped[0].ID != "orphan" {
		t.Fatalf("reaped = %+v, err = %v", reaped, err)
	}

	if _, err := s.PruneLoopRuns(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
}

// liveLoopSteps exercises the execution_id/name upsert and LoopExecution,
// which shares the loop_state row with the lease under liveLoopLease.
func liveLoopSteps(t *testing.T, s *Store) {
	if err := s.SaveLoopStep("exec-1", "step-loop", "collect", "first"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveLoopStep("exec-1", "step-loop", "collect", "second"); err != nil {
		t.Fatal(err)
	}
	result, done, err := s.LoopStep("exec-1", "collect")
	if err != nil || !done || result != "second" {
		t.Fatalf("step = %q, done=%v, err=%v, want the SECOND save (upsert)", result, done, err)
	}
	if _, done, _ := s.LoopStep("exec-1", "never-ran"); done {
		t.Error("a step that never ran reported done")
	}

	if err := s.SaveLoopStep("exec-1", "step-loop", "notify", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CompletedSteps("exec-1"); err != nil || n != 2 {
		t.Fatalf("completed = %d, err = %v, want 2", n, err)
	}
	if err := s.ClearLoopSteps("exec-1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CompletedSteps("exec-1"); n != 0 {
		t.Errorf("%d checkpoints survived ClearLoopSteps", n)
	}

	if got, err := s.LoopExecution("step-loop"); err != nil || got != "" {
		t.Fatalf("execution before set = %q, err = %v", got, err)
	}
	if err := s.SetLoopExecution("step-loop", "exec-9"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoopExecution("step-loop"); err != nil || got != "exec-9" {
		t.Errorf("execution = %q, err = %v, want exec-9", got, err)
	}
}

// liveTimers exercises due-time comparisons and the fired_at guard.
func liveTimers(t *testing.T, s *Store) {
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	if err := s.SetTimer(Timer{ID: "t1", FireAt: past, Kind: "k1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTimer(Timer{ID: "t2", FireAt: future, Kind: "k2"}); err != nil {
		t.Fatal(err)
	}

	due, err := s.DueTimers(DefaultWorkspace, time.Now(), 10)
	if err != nil || len(due) != 1 || due[0].ID != "t1" {
		t.Fatalf("due = %+v, err = %v, want only t1", due, err)
	}
	pending, err := s.PendingTimers(DefaultWorkspace, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %+v, err = %v, want 2", pending, err)
	}

	fired, err := s.MarkTimerFired("t1", time.Now())
	if err != nil || !fired {
		t.Fatalf("mark fired: fired=%v, err=%v, want true", fired, err)
	}
	if fired, err := s.MarkTimerFired("t1", time.Now()); err != nil || fired {
		t.Fatalf("mark fired twice: fired=%v, err=%v, want false (guard)", fired, err)
	}
	due, err = s.DueTimers(DefaultWorkspace, time.Now(), 10)
	if err != nil || len(due) != 0 {
		t.Errorf("due after firing t1 = %+v, want none", due)
	}

	if err := s.SetTimer(Timer{ID: "t3", Workspace: DefaultWorkspace, FireAt: past, Kind: "k3", Loop: "timer-loop"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CancelLoopTimers("timer-loop"); err != nil || n != 1 {
		t.Fatalf("cancel loop timers = %d, err = %v, want 1", n, err)
	}
	if got, err := s.TimerByID("t3"); err != sql.ErrNoRows || got != nil {
		t.Errorf("t3 after cancel = %+v, err = %v, want ErrNoRows", got, err)
	}

	if err := s.CancelTimer("t2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TimerByID("t2"); err != sql.ErrNoRows {
		t.Errorf("t2 after CancelTimer: err = %v, want ErrNoRows", err)
	}

	if _, err := s.PruneTimers(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("prune timers: %v", err)
	}
	if _, err := s.TimerByID("t1"); err != sql.ErrNoRows {
		t.Error("PruneTimers left a fired timer behind that should have aged out")
	}
}

// liveTimestampRoundTrip is the one assertion the task called out by name:
// insert a known instant in a NON-UTC zone, read it back, and check the
// instant survived — not just the wall-clock digits. Timezone skew between
// backends (server TZ, session TZ, driver TZ) is the likeliest real bug, and
// every server in this test intentionally runs in a non-UTC zone (IST) so a
// missed conversion anywhere in the chain shows up as a multi-hour error
// rather than being masked by the host already being on UTC.
func liveTimestampRoundTrip(t *testing.T, s *Store) {
	loc := time.FixedZone("TEST+0400", 4*60*60)
	// Fractional seconds land on a whole microsecond, matching the tightest
	// column precision in play (MySQL's DATETIME(6)/Postgres timestamp), so a
	// mismatch reflects a real timezone bug rather than sub-microsecond
	// rounding.
	known := time.Date(2027, 6, 18, 3, 27, 41, 589793000, loc)

	if err := s.SetTimer(Timer{ID: "ts-probe", FireAt: known, Kind: "probe"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.TimerByID("ts-probe")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	delta := got.FireAt.UTC().Sub(known.UTC())
	if delta < 0 {
		delta = -delta
	}
	if delta > time.Millisecond {
		t.Errorf("fire_at round-tripped to %v (UTC %v), want %v (UTC %v); off by %v",
			got.FireAt, got.FireAt.UTC(), known, known.UTC(), delta)
	}
}

// liveForgetOrdering pins down ForgetLeastValuable's actual ordering — not
// just "it deleted something non-pinned" but "it deleted the LOWEST-value
// non-pinned row, in importance/access_count/recency order" — against the
// derived-table rewrite (DELETE ... WHERE id IN (SELECT id FROM (SELECT ...
// LIMIT ?) AS victims)). A query that stops erroring but deletes the wrong
// rows is worse than one that fails loudly.
func liveForgetOrdering(t *testing.T, s *Store) {
	const ns = "forget-order-ns"

	// a1: higher importance, must never be picked before a lower-importance row.
	// a2/a3: same importance, tie-broken by access_count.
	// a4: lowest importance/access_count of all, but pinned — must never be
	// picked at all.
	if err := s.InsertMemoryEntry(StoredMemoryEntry{ID: "a1", AgentID: "a", Namespace: ns, Role: "user", Content: "a1", Importance: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertMemoryEntry(StoredMemoryEntry{ID: "a2", AgentID: "a", Namespace: ns, Role: "user", Content: "a2", Importance: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertMemoryEntry(StoredMemoryEntry{ID: "a3", AgentID: "a", Namespace: ns, Role: "user", Content: "a3", Importance: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertMemoryEntry(StoredMemoryEntry{ID: "a4", AgentID: "a", Namespace: ns, Role: "user", Content: "a4", Importance: 1, Pinned: true}); err != nil {
		t.Fatal(err)
	}
	// a2 gets touched twice, a3 never touched: a3 must be the one removed first
	// (same importance, lower access_count).
	if err := s.TouchMemoryEntries([]string{"a2", "a2"}); err != nil {
		t.Fatal(err)
	}

	presence := func() map[string]bool {
		list, err := s.ListMemoryEntries(ns, 10)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		m := map[string]bool{}
		for _, e := range list {
			m[e.ID] = true
		}
		return m
	}

	// Round 1: among {a1 imp2, a2 imp1 count2, a3 imp1 count0}, a3 has the
	// lowest importance AND the lowest access_count — it must go first.
	if n, err := s.ForgetLeastValuable(ns, 1); err != nil || n != 1 {
		t.Fatalf("forget round 1: n=%d err=%v, want 1", n, err)
	}
	p := presence()
	if p["a3"] {
		t.Error("round 1: a3 (lowest importance, never touched) should have been forgotten first")
	}
	for _, id := range []string{"a1", "a2", "a4"} {
		if !p[id] {
			t.Errorf("round 1: %s was forgotten but should have survived", id)
		}
	}

	// Round 2: among {a1 imp2, a2 imp1 count2}, a2 has the lower importance —
	// it must go next even though its access_count is higher than a1's zero.
	// Importance is the PRIMARY sort key, so this catches a translation that
	// silently reordered or dropped a sort column.
	if n, err := s.ForgetLeastValuable(ns, 1); err != nil || n != 1 {
		t.Fatalf("forget round 2: n=%d err=%v, want 1", n, err)
	}
	p = presence()
	if p["a2"] {
		t.Error("round 2: a2 (lower importance than a1) should have been forgotten")
	}
	if !p["a1"] {
		t.Error("round 2: a1 was forgotten but should have survived (higher importance was not yet exhausted)")
	}
	if !p["a4"] {
		t.Error("round 2: pinned a4 was forgotten")
	}

	// Round 3: only a1 is left unpinned. a4 (pinned, importance 1 — the lowest
	// possible value by every column) must never be touched.
	if n, err := s.ForgetLeastValuable(ns, 1); err != nil || n != 1 {
		t.Fatalf("forget round 3: n=%d err=%v, want 1", n, err)
	}
	p = presence()
	if p["a1"] {
		t.Error("round 3: a1 was the last unpinned row and should have been forgotten")
	}
	if !p["a4"] {
		t.Fatal("round 3: pinned a4 was forgotten — ForgetLeastValuable must never delete a pinned entry")
	}

	// Round 4: nothing unpinned left. Must be a no-op, not an error, and must
	// not touch the pinned survivor.
	if n, err := s.ForgetLeastValuable(ns, 1); err != nil || n != 0 {
		t.Errorf("forget round 4: n=%d err=%v, want 0 (nothing left to forget)", n, err)
	}
	if !presence()["a4"] {
		t.Error("round 4: the pinned entry disappeared with nothing left to legitimately forget")
	}
}

// liveDeleteOldMemoryEntries exercises the other statement the coordinator
// rewrote with the same derived-table shape (NOT IN (SELECT id FROM (...) AS
// keep)) — insert more than keepLast entries and check that exactly the
// newest keepLast survive, not merely that the right COUNT survive.
func liveDeleteOldMemoryEntries(t *testing.T, s *Store) {
	const ns = "delete-old-ns"
	ids := []string{"d1", "d2", "d3", "d4"}
	for i, id := range ids {
		if err := s.InsertMemoryEntry(StoredMemoryEntry{ID: id, AgentID: "a", Namespace: ns, Role: "user", Content: id}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
		// created_at has only whole-second resolution on SQLite's
		// datetime('now'); without a real gap between inserts, "newest 2" is
		// not a well-defined set to assert against on that backend.
		if i < len(ids)-1 {
			time.Sleep(1100 * time.Millisecond)
		}
	}

	if err := s.DeleteOldMemoryEntries(ns, 2); err != nil {
		t.Fatalf("delete old: %v", err)
	}

	list, err := s.ListMemoryEntries(ns, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]bool{}
	for _, e := range list {
		got[e.ID] = true
	}
	for _, id := range []string{"d1", "d2"} {
		if got[id] {
			t.Errorf("DeleteOldMemoryEntries kept %s, which is older than the 2 newest", id)
		}
	}
	for _, id := range []string{"d3", "d4"} {
		if !got[id] {
			t.Errorf("DeleteOldMemoryEntries removed %s, one of the 2 newest", id)
		}
	}
	if len(list) != 2 {
		t.Errorf("%d entries survived, want exactly 2", len(list))
	}
}

// liveSearchCaseInsensitive is a behaviour-parity check, not a translation
// check: SQLite's LIKE ignores ASCII case for free, so a search that matched
// under SQLite must keep matching after the move — Postgres now rewrites
// LIKE to ILIKE, and MySQL relies on the utf8mb4_unicode_ci column collation.
// If any backend disagrees with the others here, that's a real product bug
// (search results differ by which database is configured).
func liveSearchCaseInsensitive(t *testing.T, s *Store) {
	const ns = "case-ns"
	if err := s.InsertMemoryEntry(StoredMemoryEntry{
		ID: "case1", AgentID: "a", Namespace: ns, Role: "user",
		Content: "deploying workloads on Kubernetes at scale",
	}); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{"kubernetes", "KUBERNETES", "KuBeRnEtEs", "Kubernetes"} {
		found, err := s.SearchMemoryEntries(ns, q, 10)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(found) != 1 || found[0].ID != "case1" {
			t.Errorf("search %q matched %d entries, want case1 regardless of case", q, len(found))
		}
	}
	// A query with no case-insensitive match anywhere must still find nothing.
	if found, err := s.SearchMemoryEntries(ns, "openshift", 10); err != nil || len(found) != 0 {
		t.Errorf("search for an absent term matched %d entries, want 0 (err=%v)", len(found), err)
	}
}

// liveUsageByDay exercises the date(created_at) rewrite — to_char(...,
// 'YYYY-MM-DD') on Postgres, DATE_FORMAT(..., '%Y-%m-%d') on MySQL — which
// exists because DailyUsage.Day is a Go string and Postgres would otherwise
// hand back a date value and fail the scan. Usage rows are recorded with
// explicit, known UTC timestamps on two distinct days so the returned Day
// strings can be checked byte-for-byte against a fixed expectation on every
// backend, rather than merely against each other.
func liveUsageByDay(t *testing.T, s *Store) {
	day1 := time.Date(2027, 1, 10, 22, 15, 0, 0, time.UTC)
	day2 := time.Date(2027, 1, 11, 3, 0, 0, 0, time.UTC)

	if err := s.RecordModelUsage(ModelUsage{
		AgentID: "a", Provider: "anthropic", Model: "model-a", Kind: "main",
		InputTokens: 100, OutputTokens: 50, CreatedAt: day1,
	}); err != nil {
		t.Fatalf("record day1: %v", err)
	}
	if err := s.RecordModelUsage(ModelUsage{
		AgentID: "a", Provider: "anthropic", Model: "model-b", Kind: "main",
		InputTokens: 200, OutputTokens: 75, CreatedAt: day2,
	}); err != nil {
		t.Fatalf("record day2: %v", err)
	}

	usage, err := s.UsageByDay(day1.Add(-time.Hour))
	if err != nil {
		t.Fatalf("usage by day: %v", err)
	}
	if len(usage) != 2 {
		t.Fatalf("usage = %+v, want 2 day/model rows", usage)
	}
	if usage[0].Day != "2027-01-10" {
		t.Errorf("day 1 = %q, want 2027-01-10 (date() translation)", usage[0].Day)
	}
	if usage[1].Day != "2027-01-11" {
		t.Errorf("day 2 = %q, want 2027-01-11 (date() translation)", usage[1].Day)
	}
	if usage[0].InputTokens != 100 || usage[1].InputTokens != 200 {
		t.Errorf("usage = %+v, wrong tokens attributed to the wrong day", usage)
	}
}

// turnByEventID finds one turn in a RecentTurns result.
func turnByEventID(turns []AgentTurn, eventID string) (AgentTurn, bool) {
	for _, t := range turns {
		if t.EventID == eventID {
			return t, true
		}
	}
	return AgentTurn{}, false
}

// liveStartAgentTurn exercises startTurnSQL's MySQL-specific CASE-WHEN form
// (no ON DUPLICATE KEY has a WHERE, so the guard moves into every assignment)
// against the portable ON CONFLICT ... WHERE form Postgres/SQLite use — the
// two must agree on every observable: whether the caller owns the turn,
// whether attempt actually incremented, and whether finished_at/error were
// cleared on resume.
func liveStartAgentTurn(t *testing.T, s *Store) {
	const agentID = "turn-agent"
	const eventID = "evt-1"

	ok, err := s.StartAgentTurn(AgentTurn{
		ID: "turn-1", AgentID: agentID, EventID: eventID, EventKind: "k",
		EventJSON: "{}", Attempt: 1, StartedAt: time.Now(),
	})
	if err != nil || !ok {
		t.Fatalf("first start: ok=%v err=%v, want true", ok, err)
	}

	// A redelivery while the turn is still running must be told it's a
	// duplicate, and must NOT bump attempt.
	ok, err = s.StartAgentTurn(AgentTurn{
		ID: "turn-1-redelivered", AgentID: agentID, EventID: eventID, EventKind: "k",
		EventJSON: "{}", Attempt: 1, StartedAt: time.Now(),
	})
	if err != nil || ok {
		t.Fatalf("start while running: ok=%v err=%v, want false", ok, err)
	}
	turns, err := s.RecentTurns(agentID, 10)
	if err != nil {
		t.Fatalf("recent turns: %v", err)
	}
	turn, found := turnByEventID(turns, eventID)
	if !found || turn.Attempt != 1 {
		t.Fatalf("after a duplicate start: turn = %+v, found=%v, want attempt 1", turn, found)
	}

	// Fail the turn, then confirm a start resumes it: true, attempt bumped by
	// exactly one, status back to running, finished_at/error cleared.
	if err := s.FinishAgentTurn(eventID, TurnFailed, "boom"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	turns, _ = s.RecentTurns(agentID, 10)
	turn, _ = turnByEventID(turns, eventID)
	if turn.Status != TurnFailed || turn.FinishedAt == nil || turn.Error != "boom" {
		t.Fatalf("after finish: turn = %+v, want failed/finished/boom", turn)
	}

	ok, err = s.StartAgentTurn(AgentTurn{
		ID: "turn-1-retry", AgentID: agentID, EventID: eventID, EventKind: "k",
		EventJSON: "{}", Attempt: 1, StartedAt: time.Now(),
	})
	if err != nil || !ok {
		t.Fatalf("resume after failure: ok=%v err=%v, want true", ok, err)
	}
	turns, _ = s.RecentTurns(agentID, 10)
	turn, found = turnByEventID(turns, eventID)
	if !found {
		t.Fatal("turn vanished after resume")
	}
	if turn.Attempt != 2 {
		t.Errorf("attempt after resume = %d, want exactly 2 (1 + 1, not the caller's Attempt field)", turn.Attempt)
	}
	if turn.Status != TurnRunning {
		t.Errorf("status after resume = %q, want %q", turn.Status, TurnRunning)
	}
	if turn.FinishedAt != nil {
		t.Errorf("finished_at after resume = %v, want cleared to nil", turn.FinishedAt)
	}
	if turn.Error != "" {
		t.Errorf("error after resume = %q, want cleared", turn.Error)
	}

	// A second redelivery while the RESUMED turn is running must again be
	// refused, and must not bump attempt a second time.
	ok, err = s.StartAgentTurn(AgentTurn{
		ID: "turn-1-redelivered-2", AgentID: agentID, EventID: eventID, EventKind: "k",
		EventJSON: "{}", Attempt: 1, StartedAt: time.Now(),
	})
	if err != nil || ok {
		t.Fatalf("start while resumed-and-running: ok=%v err=%v, want false", ok, err)
	}
	turns, _ = s.RecentTurns(agentID, 10)
	turn, _ = turnByEventID(turns, eventID)
	if turn.Attempt != 2 {
		t.Errorf("attempt after a second duplicate = %d, want unchanged 2", turn.Attempt)
	}
}
