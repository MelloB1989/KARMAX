package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

const poolYAML = `home: /h
hosts: {kali: {exec: [docker]}, pc2: {exec: [docker, --context, pc2]}}
orchestrator: {host: kali, token_env: O}
slots: 1
agents:
  agent-01: {host: kali, token_env: T1}
  agent-02: {host: pc2, token_env: T2}
  agent-03: {host: pc2, token_env: T3}
`

// newPool is a fleet of three agents, all standby with no quota use.
func newPool(t *testing.T) (*Fleet, map[string]*box) {
	t.Helper()
	cfg, err := config.Parse([]byte(poolYAML))
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = t.TempDir()
	db, err := ledger.Open(filepath.Join(cfg.StateDir, "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	boxes := map[string]*box{}
	for _, n := range []string{"agent-01", "agent-02", "agent-03", "karmax"} {
		boxes[n] = &box{transcript: map[string]string{}, obs: healthy(n)}
	}
	out := &outbox{}
	f, err := New(cfg, db, func(name string) Box { return boxes[name] }, out, out)
	if err != nil {
		t.Fatal(err)
	}
	f.waitEvery = 5 * time.Millisecond
	f.relayBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	f.soonEvery = 0
	return f, boxes
}

// set rewrites one agent's state.
func set(f *Fleet, name string, edit func(*reconcile.State)) {
	st := f.State(name)
	edit(&st)
	f.setState(st)
}

func TestPickPrefersRepoAffinity(t *testing.T) {
	f, _ := newPool(t)
	// agent-03 has the most quota used, but its last task was in the repo.
	set(f, "agent-03", func(s *reconcile.State) { s.Quota.FiveHour = 80 })
	set(f, "agent-01", func(s *reconcile.State) { s.Quota.FiveHour = 5 })
	if err := f.db.OpenTask("old", "agent-03", "operator", t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetTaskRepo("old", "/code/site"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CloseTask("old", t0, "done", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := f.Pick("/code/site", t0); got.Agent != "agent-03" || !strings.Contains(got.Why, "/code/site") {
		t.Errorf("with repo: %+v", got)
	}
	if got := f.Pick("", t0); got.Agent != "agent-02" && got.Agent != "agent-01" {
		t.Errorf("without repo the quota decides: %+v", got)
	}
	if got := f.Pick("other", t0); got.Agent == "agent-03" {
		t.Errorf("no affinity for another repo: %+v", got)
	}
}

func TestPickQuotaThenIdle(t *testing.T) {
	f, _ := newPool(t)
	set(f, "agent-01", func(s *reconcile.State) { s.Quota.FiveHour, s.Quota.SevenDay = 40, 10; s.LastBusy = t0.Add(-time.Hour) })
	set(f, "agent-02", func(s *reconcile.State) { s.Quota.FiveHour, s.Quota.SevenDay = 10, 50; s.LastBusy = t0.Add(-time.Hour) })
	set(f, "agent-03", func(s *reconcile.State) {
		s.Quota.FiveHour, s.Quota.SevenDay = 10, 20
		s.LastBusy = t0.Add(-time.Minute)
	})
	r := f.Pick("", t0)
	if r.Agent != "agent-03" {
		t.Fatalf("lowest 5h, then lowest 7d: %+v", r)
	}
	if len(r.Candidates) != 3 || r.Candidates[0].Agent != "agent-03" || !r.Candidates[0].Eligible {
		t.Errorf("chosen candidate first: %+v", r.Candidates)
	}
	// Same quota everywhere: the longest idle wins.
	for _, n := range []string{"agent-01", "agent-02", "agent-03"} {
		set(f, n, func(s *reconcile.State) { s.Quota.FiveHour, s.Quota.SevenDay = 10, 10 })
	}
	set(f, "agent-02", func(s *reconcile.State) { s.LastBusy = t0.Add(-5 * time.Hour) })
	if r := f.Pick("", t0); r.Agent != "agent-02" {
		t.Errorf("longest idle: %+v", r)
	}
}

func TestPickExcludes(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*reconcile.State)
		why  string
	}{
		{"working", func(s *reconcile.State) { s.Phase = reconcile.Working }, "working"},
		{"waiting with an open task", func(s *reconcile.State) { s.Phase, s.Task = reconcile.Waiting, "T1" }, "T1"},
		{"standby with an open task", func(s *reconcile.State) { s.Task = "T1" }, "T1"},
		{"cooling", func(s *reconcile.State) { s.Phase = reconcile.Cooling }, "cooling"},
		{"down", func(s *reconcile.State) { s.Phase = reconcile.Down }, "down"},
		{"disabled", func(s *reconcile.State) { s.Phase = reconcile.Disabled }, "disabled"},
		{"low", func(s *reconcile.State) { s.Low = true }, "low"},
		{"reserved", func(s *reconcile.State) { s.Reserved = true }, "reserved"},
		// Rotated: its fresh session is not up yet, and a brief would find no
		// one to take it.
		{"starting a session", func(s *reconcile.State) { s.SessionID, s.RecordedAt = "", t0 }, "starting"},
		// Done, and not yet rotated: a task now would run in the last task's
		// session, with its context.
		{"done, not yet rotated", func(s *reconcile.State) { s.Task, s.Done, s.SessionID = "T0", true, "s1" }, "rotat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newPool(t)
			set(f, "agent-01", tc.edit)
			set(f, "agent-02", func(s *reconcile.State) { s.Phase = reconcile.Down })
			set(f, "agent-03", func(s *reconcile.State) { s.Phase = reconcile.Down })
			r := f.Pick("", t0)
			if r.Agent != "" {
				t.Fatalf("picked %s", r.Agent)
			}
			var c *Candidate
			for i := range r.Candidates {
				if r.Candidates[i].Agent == "agent-01" {
					c = &r.Candidates[i]
				}
			}
			if c == nil || c.Eligible || !strings.Contains(c.Why, tc.why) {
				t.Errorf("candidate = %+v, want why containing %q", c, tc.why)
			}
		})
	}
}

func TestPickWaitingWithClosedTaskIsEligible(t *testing.T) {
	f, _ := newPool(t)
	for _, n := range []string{"agent-02", "agent-03"} {
		set(f, n, func(s *reconcile.State) { s.Phase = reconcile.Down })
	}
	set(f, "agent-01", func(s *reconcile.State) { s.Phase, s.Task, s.Done = reconcile.Waiting, "T1", true })
	if r := f.Pick("", t0); r.Agent != "" {
		t.Errorf("picked before its rotation, into the closed task's session: %+v", r)
	}
	// The idle tick rotates it: a closed task holds the agent no longer.
	f.Tick(context.Background(), t0)
	set(f, "agent-02", func(s *reconcile.State) { s.Phase = reconcile.Down })
	set(f, "agent-03", func(s *reconcile.State) { s.Phase = reconcile.Down })
	set(f, "agent-01", func(s *reconcile.State) { s.SessionID = "s-new" }) // its fresh session, seen
	if r := f.Pick("", t0); r.Agent != "agent-01" {
		t.Errorf("a closed task still holds the agent after its rotation: %+v", r)
	}
}

func TestPickIgnoresAnOpenTaskTheStateForgot(t *testing.T) {
	f, _ := newPool(t)
	for _, n := range []string{"agent-02", "agent-03"} {
		set(f, n, func(s *reconcile.State) { s.Phase = reconcile.Down })
	}
	if err := f.db.OpenTask("T9", "agent-01", "operator", t0); err != nil {
		t.Fatal(err)
	}
	if r := f.Pick("", t0); r.Agent != "" {
		t.Errorf("the ledger has an open task for agent-01: %+v", r)
	}
}

func TestRunConcurrentPicksDiffer(t *testing.T) {
	f, _ := newPool(t)
	set(f, "agent-03", func(s *reconcile.State) { s.Phase = reconcile.Down })
	var wg sync.WaitGroup
	reps := make([]RunReply, 2)
	errs := make([]error, 2)
	for i := range reps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reps[i], errs[i] = f.Run(context.Background(), RunRequest{Brief: "do a thing", From: "operator"}, t0)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || reps[0].Agent == reps[1].Agent || reps[0].Task == reps[1].Task {
		t.Fatalf("two free agents, two runs: %+v %v", reps, errs)
	}

	// One free agent: one run gets it, the other a conflict.
	f, _ = newPool(t)
	set(f, "agent-02", func(s *reconcile.State) { s.Phase = reconcile.Down })
	set(f, "agent-03", func(s *reconcile.State) { s.Phase = reconcile.Down })
	for i := range reps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reps[i], errs[i] = f.Run(context.Background(), RunRequest{Brief: "do a thing"}, t0)
		}()
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, err := range errs {
		var nf *NoFreeAgent
		switch {
		case err == nil:
			ok++
		case errors.As(err, &nf) && errors.Is(err, ErrConflict) && len(nf.Candidates) == 3:
			conflict++
		default:
			t.Errorf("err = %v", err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Errorf("ok=%d conflict=%d", ok, conflict)
	}
}

func TestRunNamedBusyAgent409(t *testing.T) {
	f, boxes := newPool(t)
	set(f, "agent-02", func(s *reconcile.State) { s.Phase, s.Task = reconcile.Working, "T1" })
	_, err := f.Run(context.Background(), RunRequest{Brief: "x", Agent: "agent-02"}, t0)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "agent-02") || !strings.Contains(err.Error(), "T1") {
		t.Fatalf("err = %v", err)
	}
	for n, b := range boxes {
		if len(b.calls) != 0 {
			t.Errorf("%s was touched: %v", n, b.calls)
		}
	}
	// A free named agent is taken even when another would rank higher.
	rep, err := f.Run(context.Background(), RunRequest{Brief: "x", Agent: "agent-03"}, t0)
	if err != nil || rep.Agent != "agent-03" {
		t.Errorf("rep = %+v, %v", rep, err)
	}
	if _, err := f.Run(context.Background(), RunRequest{Brief: "x", Agent: "nobody"}, t0); !errors.Is(err, ErrBadInput) {
		t.Errorf("unknown agent: %v", err)
	}
}

func TestRunComposesBrief(t *testing.T) {
	f, boxes := newPool(t)
	rep, err := f.Run(context.Background(), RunRequest{Brief: "fix the bug\nthen test", Repo: "KARMAX", Agent: "agent-01", From: "karmax", Task: "T7"}, t0)
	if err != nil || rep.Task != "T7" {
		t.Fatalf("rep = %+v, %v", rep, err)
	}
	want := "relay karmax→agent-01: Task T7 from karmax. Repo: KARMAX. fix the bug\nthen test"
	if got := boxes["agent-01"].calls; len(got) != 1 || got[0] != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
	tk, err := f.db.Task("T7")
	if err != nil || tk.Agent != "agent-01" || tk.Assigner != "karmax" || tk.Repo != "KARMAX" || !tk.Closed.IsZero() {
		t.Errorf("task = %+v, %v", tk, err)
	}
	if st := f.State("agent-01"); st.Task != "T7" {
		t.Errorf("state task = %q", st.Task)
	}

	// No repo: no repo sentence; the sender defaults to the operator.
	if _, err := f.Run(context.Background(), RunRequest{Brief: "hello", Agent: "agent-02", Task: "T8"}, t0); err != nil {
		t.Fatal(err)
	}
	if got := boxes["agent-02"].calls; len(got) != 1 || got[0] != "relay operator→agent-02: Task T8 from operator. hello" {
		t.Errorf("calls = %q", got)
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	f, _ := newPool(t)
	for _, in := range []RunRequest{{Brief: "  "}, {Brief: "x", Repo: "../etc"}, {Brief: "x", Repo: "a/../b"}} {
		if _, err := f.Run(context.Background(), in, t0); !errors.Is(err, ErrBadInput) {
			t.Errorf("%+v: err = %v", in, err)
		}
	}
}

type failRelay struct{ *box }

func (failRelay) Relay(context.Context, string, string, string) error { return errors.New("pane gone") }

func TestRunUndeliveredClosesTask(t *testing.T) {
	f, _ := newPool(t)
	inner := f.box
	f.box = func(name string) Box {
		if name == "agent-01" {
			return failRelay{inner(name).(*box)}
		}
		return inner(name)
	}
	_, err := f.Run(context.Background(), RunRequest{Brief: "x", Agent: "agent-01", Task: "T1"}, t0)
	if err == nil || !strings.Contains(err.Error(), "pane gone") {
		t.Fatalf("err = %v", err)
	}
	tk, _ := f.db.Task("T1")
	if tk.Closed.IsZero() || tk.Outcome != "undelivered" {
		t.Errorf("task = %+v", tk)
	}
	if st := f.State("agent-01"); st.Task != "" {
		t.Errorf("the agent still holds %q", st.Task)
	}
}

func TestTaskIDShape(t *testing.T) {
	re := regexp.MustCompile(`^20261012-[a-z0-9-]{1,32}-[0-9a-f]{4}$`)
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	for brief, slug := range map[string]string{
		"Fix the Login Bug, please!": "fix-the-login-bug",
		"  one":                      "one",
		"!!! ???":                    "task",
		"supercalifragilisticexpialidocious antidisestablishmentarianism x y": "supercalifragilisticexpialidocious"[:32-0],
	} {
		id := TaskID(brief, now)
		if !re.MatchString(id) {
			t.Errorf("%q → %q", brief, id)
			continue
		}
		if !strings.HasPrefix(id, "20261012-"+slug[:min(len(slug), 20)]) {
			t.Errorf("%q → %q, want slug %q", brief, id, slug)
		}
	}
	if TaskID("same", now) == TaskID("same", now) {
		t.Error("two ids for one brief collided")
	}
}

func TestRunQueueWaitsForFree(t *testing.T) {
	f, _ := newPool(t)
	for _, n := range []string{"agent-01", "agent-02", "agent-03"} {
		set(f, n, func(s *reconcile.State) { s.Phase = reconcile.Working })
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		set(f, "agent-02", func(s *reconcile.State) { s.Phase = reconcile.Standby })
	}()
	rep, err := f.Run(context.Background(), RunRequest{Brief: "x", Queue: 5 * time.Second}, t0)
	if err != nil || rep.Agent != "agent-02" {
		t.Fatalf("rep = %+v, %v", rep, err)
	}

	// The queue time ends: a conflict, not a hang. A cancelled context too.
	set(f, "agent-02", func(s *reconcile.State) { s.Phase = reconcile.Working; s.Task = "" })
	start := time.Now()
	_, err = f.Run(context.Background(), RunRequest{Brief: "x", Queue: 50 * time.Millisecond}, t0)
	if !errors.Is(err, ErrConflict) || time.Since(start) > 2*time.Second {
		t.Errorf("err = %v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err = f.Run(ctx, RunRequest{Brief: "x", Queue: time.Hour}, t0); err == nil {
		t.Error("a cancelled context should end the queue")
	}
}

// The brief run delivers is the first message of the task's conversation, so
// result shows it and wait --task waits for the answer to it.
func TestRunFilesTheBriefUnderTheTask(t *testing.T) {
	f, _ := newPool(t)
	if _, err := f.Run(context.Background(), RunRequest{Brief: "do it", Agent: "agent-01", Task: "T9"}, t0); err != nil {
		t.Fatal(err)
	}
	thread, err := f.db.TaskThread("T9")
	if err != nil || len(thread) != 1 || thread[0].To != "agent-01" || !strings.HasSuffix(thread[0].Text, "do it") {
		t.Fatalf("thread = %+v, %v", thread, err)
	}
}

// A run whose answer was lost (fleetd restarted, the connection dropped) is
// tried again by a careful caller with the same task id. It must not hand the
// task to a second agent: it answers with the first.
func TestARunRetriedWithItsTaskIDIsNotAssignedTwice(t *testing.T) {
	f, boxes := newPool(t)
	in := RunRequest{Brief: "fix it", Task: "T1"}
	first, err := f.Run(context.Background(), in, t0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.Run(context.Background(), in, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if again.Agent != first.Agent || again.Task != "T1" {
		t.Fatalf("first %+v, again %+v", first, again)
	}
	held := 0
	for _, n := range []string{"agent-01", "agent-02", "agent-03"} {
		if f.State(n).Task == "T1" {
			held++
		}
		relays := 0
		for _, c := range boxes[n].calls {
			if strings.HasPrefix(c, "relay ") {
				relays++
			}
		}
		if relays > 1 || (relays == 1 && n != first.Agent) {
			t.Errorf("%s got %d briefs", n, relays)
		}
	}
	if held != 1 {
		t.Fatalf("%d agents hold T1", held)
	}

	// A closed task is not run again under its id, except one whose brief
	// never got there.
	_ = f.Done("", "T1", "done", "", "", t0.Add(time.Hour))
	if _, err := f.Run(context.Background(), in, t0.Add(2*time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatalf("rerun of a closed task: %v", err)
	}
	boxes["agent-01"].relayErr = errors.New("no session")
	boxes["agent-02"].relayErr = errors.New("no session")
	boxes["agent-03"].relayErr = errors.New("no session")
	in2 := RunRequest{Brief: "other", Task: "T2"}
	if _, err := f.Run(context.Background(), in2, t0); err == nil {
		t.Fatal("undelivered run succeeded")
	}
	boxes["agent-01"].relayErr, boxes["agent-02"].relayErr, boxes["agent-03"].relayErr = nil, nil, nil
	if _, err := f.Run(context.Background(), in2, t0.Add(time.Minute)); err != nil {
		t.Fatalf("rerun of an undelivered task: %v", err)
	}
}

// Done is followed by the rotation within seconds, not at the next tick: the
// agent is out of the pool until it has its fresh session.
func TestDoneRotatesSoonAfterTheTurnEnds(t *testing.T) {
	f, boxes := newPool(t)
	f.soonEvery = 5 * time.Millisecond
	f.Tick(context.Background(), t0)
	b := boxes["agent-01"]
	b.mu.Lock()
	b.obs.Sessions[0].Status = "busy" // done lands while it writes its last words
	b.mu.Unlock()
	if _, err := f.Run(context.Background(), RunRequest{Brief: "x", Agent: "agent-01", Task: "T1"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := f.Done("", "T1", "done", "", "", t0); err != nil {
		t.Fatal(err)
	}
	if p := f.Pick("", t0); slices.ContainsFunc(p.Candidates, func(c Candidate) bool { return c.Agent == "agent-01" && c.Eligible }) {
		t.Fatal("eligible before its rotation")
	}
	time.Sleep(30 * time.Millisecond)
	b.mu.Lock()
	b.obs.Sessions[0].Status = "idle"
	b.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for f.State("agent-01").Done && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.State("agent-01").Done || !slices.ContainsFunc(b.snapshotCalls(), func(c string) bool { return c == "fresh" }) {
		t.Fatalf("not rotated: %+v calls %q", f.State("agent-01"), b.snapshotCalls())
	}
}
