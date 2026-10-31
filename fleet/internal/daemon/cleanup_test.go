package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/archive"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// cleanups fakes where the cleanup script runs: in each agent's container,
// and on fleetd's machine for the shared code's worktrees. It records what
// each run was asked and answers with the lines it is given.
type cleanups struct {
	mu    sync.Mutex
	in    map[string][]string // agent → the script's args, last run
	runs  map[string]int
	here  []string          // the shared code's dirs, last run
	out   map[string]string // agent, or "code" → the script's output
	err   map[string]error
	block chan struct{} // when set, every run in a container waits on it
}

func fakeCleanups(f *Fleet) *cleanups {
	c := &cleanups{in: map[string][]string{}, runs: map[string]int{}, out: map[string]string{}, err: map[string]error{}}
	f.cleanIn = func(ctx context.Context, worker, script string, args ...string) ([]byte, error) {
		c.mu.Lock()
		blk := c.block
		c.in[worker], c.runs[worker] = args, c.runs[worker]+1
		out, err := c.out[worker], c.err[worker]
		c.mu.Unlock()
		if blk != nil {
			<-blk
		}
		return []byte(out), err
	}
	f.cleanHere = func(ctx context.Context, args ...string) ([]byte, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.here = args[3:]
		return []byte(c.out["code"]), nil
	}
	return c
}

// oldArchive writes a session archive made at, for task (closed when closed).
func oldArchive(t *testing.T, f *Fleet, id, task string, at time.Time) string {
	t.Helper()
	dir := filepath.Join(f.archiveRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(archive.Manifest{ID: id, Agent: strings.Split(id, "/")[0], Task: task, ArchivedAt: at})
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl.zst"), make([]byte, 3000), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestACleanupPassLeavesAgentsAtWorkAloneAndTellsWhatItKept(t *testing.T) {
	f, _, out := newSlots(t)
	f.cfg.Code = t.TempDir()
	for _, d := range []string{"agent-01", "agent-01-2", "agent-02", "someone"} {
		if err := os.MkdirAll(filepath.Join(f.cfg.Code, ".worktrees", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := fakeCleanups(f)
	c.out["agent-01"] = "deleted\t2048\t/work/scratch\tunused\nkept\t10\t/work/wt/x\t1 commit no remote has in /work/wt/x\n"
	c.out["code"] = "deleted\t4096\t" + f.cfg.Code + "/.worktrees/agent-01/jdk\tunused\n"
	set(f, "agent-02:2", func(s *reconcile.State) { s.Task = "T9" })
	now := t0.Add(30 * 24 * time.Hour)
	_ = f.db.OpenTask("T-old", "agent-01", "operator", t0)
	_ = f.db.CloseTask("T-old", t0.Add(time.Hour), "done", "", "")
	_ = f.db.OpenTask("T-open", "agent-02:2", "operator", t0)
	gone := oldArchive(t, f, "agent-01/2026-10-01-aaaaaaaa", "T-old", now.Add(-10*24*time.Hour))
	open := oldArchive(t, f, "agent-02:2/2026-10-01-bbbbbbbb", "T-open", now.Add(-10*24*time.Hour)) // its task is open
	recent := oldArchive(t, f, "agent-01/2026-10-30-cccccccc", "T-old", now.Add(-24*time.Hour))

	rep, err := f.Cleanup(context.Background(), false, now)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.in["agent-01"], []string{itoa(int(now.Add(-7 * 24 * time.Hour).Unix())), "0", "work"}) {
		t.Errorf("agent-01's container was asked %q", c.in["agent-01"])
	}
	if c.runs["agent-02"] != 0 || !slices.ContainsFunc(rep.Skipped, func(s fleetapi.CleanupSkip) bool {
		return s.Where == "agent-02" && strings.Contains(s.Why, "agent-02:2 has task T9")
	}) {
		t.Errorf("agent-02 is at work: ran %d, skipped %+v", c.runs["agent-02"], rep.Skipped)
	}
	wt := filepath.Join(f.cfg.Code, ".worktrees")
	if !slices.Equal(c.here, []string{wt + "/agent-01", wt + "/agent-01-2"}) {
		t.Errorf("the shared code's dirs: %q, want agent-01's only", c.here)
	}
	if _, err := os.Stat(gone); err == nil {
		t.Error("an archive older than a week, of a closed task, is still there")
	}
	for _, d := range []string{open, recent} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
	want := map[string]string{"agent-01 /work/scratch": "deleted", "agent-01 /work/wt/x": "kept",
		"code " + wt + "/agent-01/jdk": "deleted", "archive agent-01/2026-10-01-aaaaaaaa": "deleted"}
	for _, it := range rep.Items {
		k := it.Where + " " + it.Path
		if want[k] != it.Action {
			t.Errorf("item %s: %s, want %q", k, it.Action, want[k])
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Errorf("not reported: %v", want)
	}
	if rep.Freed < (2048+4096)<<10+3000 {
		t.Errorf("freed %d", rep.Freed)
	}
	evs, _ := f.db.EventsWhere(ledger.EventQuery{Kinds: []string{"fleet.cleanup"}})
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, `"freed_bytes"`) || !strings.Contains(evs[0].Detail, "/work/wt/x") {
		t.Fatalf("events: %+v", evs)
	}
	if len(out.alerts) != 1 || !strings.Contains(out.alerts[0], "agent-01") || !strings.Contains(out.alerts[0], "/work/wt/x") ||
		!strings.Contains(out.alerts[0], "1 commit no remote has") {
		t.Fatalf("alerts: %q", out.alerts)
	}

	// Kept again the next day: you were told already.
	if _, err := f.Cleanup(context.Background(), false, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(out.alerts) != 1 {
		t.Errorf("told again: %q", out.alerts)
	}
}

func TestACleanupDryRunDeletesAndRecordsNothing(t *testing.T) {
	f, _, out := newSlots(t)
	c := fakeCleanups(f)
	c.out["agent-01"] = "would\t2048\t/work/scratch\tunused\nkept\t10\t/work/wt/x\tuncommitted changes in /work/wt/x\n"
	now := t0.Add(30 * 24 * time.Hour)
	_ = f.db.OpenTask("T-old", "agent-01", "operator", t0)
	_ = f.db.CloseTask("T-old", t0.Add(time.Hour), "done", "", "")
	arch := oldArchive(t, f, "agent-01/2026-10-01-aaaaaaaa", "T-old", now.Add(-10*24*time.Hour))

	rep, err := f.Cleanup(context.Background(), true, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.in["agent-01"][1] != "1" {
		t.Errorf("the container was not asked for a dry run: %q", c.in["agent-01"])
	}
	if _, err := os.Stat(arch); err != nil {
		t.Errorf("a dry run deleted an archive: %v", err)
	}
	i := slices.IndexFunc(rep.Items, func(it fleetapi.CleanupItem) bool { return it.Where == "archive" })
	if i < 0 || rep.Items[i].Action != fleetapi.CleanupWould || !rep.DryRun || rep.Freed < 2048<<10 {
		t.Errorf("report: %+v", rep)
	}
	evs, _ := f.db.EventsWhere(ledger.EventQuery{Kinds: []string{"fleet.cleanup"}})
	if len(evs) != 0 || len(out.alerts) != 0 {
		t.Errorf("a dry run recorded %+v and told %q", evs, out.alerts)
	}
}

// fleetd runs it on its own: not in its first minutes (a restart brings every
// agent back at once), then every cleanup.every, one pass at a time.
func TestTheCleanupRunsOnItsScheduleOnePassAtATime(t *testing.T) {
	f, _, _ := newSlots(t)
	c := fakeCleanups(f)
	f.Started = t0
	ran := func() int { c.mu.Lock(); defer c.mu.Unlock(); return c.runs["agent-01"] }

	f.Tick(context.Background(), t0.Add(time.Minute))
	f.cleanWG.Wait()
	if ran() != 0 {
		t.Fatal("a pass ran in fleetd's first minutes")
	}
	f.Tick(context.Background(), t0.Add(20*time.Minute))
	f.cleanWG.Wait()
	f.Tick(context.Background(), t0.Add(2*time.Hour))
	f.cleanWG.Wait()
	if ran() != 1 {
		t.Fatalf("ran %d times in its first day, want once", ran())
	}
	f.Tick(context.Background(), t0.Add(25*time.Hour))
	f.cleanWG.Wait()
	if ran() != 2 {
		t.Fatalf("ran %d times, want a second pass a day later", ran())
	}

	// While a pass runs, fleetctl cleanup is told so.
	c.block = make(chan struct{})
	f.Tick(context.Background(), t0.Add(50*time.Hour))
	deadline := time.Now().Add(5 * time.Second)
	for ran() != 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.Cleanup(context.Background(), true, t0.Add(50*time.Hour)); !errors.Is(err, ErrConflict) {
		t.Errorf("a second pass at once: %v", err)
	}
	close(c.block)
	f.cleanWG.Wait()

	// A fleetd restarted soon after a pass does not run another.
	g, err := New(f.cfg, f.db, f.box, f.push, f.notify)
	if err != nil {
		t.Fatal(err)
	}
	g.Started = t0.Add(50 * time.Hour)
	if !g.lastCleanup.Equal(t0.Add(50 * time.Hour).Truncate(time.Second)) {
		t.Errorf("last pass remembered as %v", g.lastCleanup)
	}

	off := false
	f.cfg.Cleanup.Enabled = &off
	f.Tick(context.Background(), t0.Add(100*time.Hour))
	f.cleanWG.Wait()
	if ran() != 3 {
		t.Errorf("cleanup: {enabled: false} ran it")
	}
}

// An agent fleetd cannot reach is not waited on; one whose container fails
// the script is said, and the others are cleaned all the same.
func TestACleanupPassGoesOnWithoutAnAgentItCannotReach(t *testing.T) {
	f, _, _ := newSlots(t)
	c := fakeCleanups(f)
	set(f, "agent-02", func(s *reconcile.State) { s.UnreachableSince = t0 })
	c.err["agent-01"] = errors.New("exit 1: no /work")
	rep, err := f.Cleanup(context.Background(), false, t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if c.runs["agent-02"] != 0 {
		t.Error("ran in an agent out of reach")
	}
	for _, w := range []string{"agent-01", "agent-02"} {
		if !slices.ContainsFunc(rep.Skipped, func(s fleetapi.CleanupSkip) bool { return s.Where == w }) {
			t.Errorf("%s not said: %+v", w, rep.Skipped)
		}
	}
}

// A host's agents are cleaned one after another, and a pass can take a while:
// an agent given a task meanwhile is left alone when its turn comes.
func TestAnAgentGivenATaskDuringAPassIsLeftAlone(t *testing.T) {
	f, _ := newPool(t)
	f.cfg.Code = t.TempDir()
	c := fakeCleanups(f)
	in := f.cleanIn
	f.cleanIn = func(ctx context.Context, worker, script string, args ...string) ([]byte, error) {
		if worker == "agent-02" { // agent-03, on the same host, is next
			set(f, "agent-03", func(s *reconcile.State) { s.Task = "T-new" })
		}
		return in(ctx, worker, script, args...)
	}

	rep, err := f.Cleanup(context.Background(), true, t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if c.runs["agent-02"] != 1 || c.runs["agent-03"] != 0 {
		t.Errorf("runs: agent-02 %d, agent-03 %d; want agent-03 left alone", c.runs["agent-02"], c.runs["agent-03"])
	}
	if !slices.ContainsFunc(rep.Skipped, func(s fleetapi.CleanupSkip) bool {
		return s.Where == "agent-03" && strings.Contains(s.Why, "has task T-new")
	}) {
		t.Errorf("skipped: %+v", rep.Skipped)
	}
}
