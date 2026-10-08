package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/agent"
	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/observe"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// box is a fake agent container.
type box struct {
	mu         sync.Mutex
	obs        reconcile.Obs
	transcript map[string]string // sid → body
	badHash    bool
	wts        []agent.Worktree
	calls      []string
	acked      int
}

func (b *box) log(s string) { b.mu.Lock(); b.calls = append(b.calls, s); b.mu.Unlock() }

func (b *box) Observe(_ context.Context, now time.Time) (reconcile.Obs, error) {
	o := b.obs
	o.Now = now
	return o, nil
}
func (b *box) AckEvents(context.Context) error             { b.acked++; return nil }
func (b *box) Restart(_ context.Context, sid string) error { b.log("restart " + sid); return nil }
func (b *box) Fresh(context.Context) error                 { b.log("fresh"); return nil }
func (b *box) Prompt(_ context.Context, t string) error    { b.log("prompt " + t); return nil }
func (b *box) StopPID(_ context.Context, pid int) error    { b.log("stop " + itoa(pid)); return nil }
func (b *box) DeleteTranscript(_ context.Context, sid string) error {
	b.log("delete " + sid)
	return nil
}
func (b *box) RemoveWorktree(_ context.Context, w agent.Worktree) error {
	b.log("rmwt " + w.Path)
	return nil
}
func (b *box) PullTranscript(_ context.Context, sid string) (*agent.Transcript, error) {
	body, ok := b.transcript[sid]
	if !ok {
		return nil, agent.ErrNoTranscript
	}
	return &agent.Transcript{Project: "-work", Main: []byte(body), Sub: map[string][]byte{}}, nil
}
func (b *box) HashTranscript(_ context.Context, sid string) (string, error) {
	body, ok := b.transcript[sid]
	if !ok {
		return "", agent.ErrNoTranscript
	}
	if b.badHash {
		body += "x"
	}
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:]), nil
}
func (b *box) Worktrees(context.Context) ([]agent.Worktree, error) { return b.wts, nil }
func (b *box) PushTranscript(_ context.Context, sid string, _ *agent.Transcript) error {
	b.log("push " + sid)
	return nil
}
func (b *box) Relay(_ context.Context, to, from, text string) error {
	b.log("relay " + from + "→" + to + ": " + text)
	return nil
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

type outbox struct {
	mu     sync.Mutex
	pushes []string
	alerts []string
}

func (o *outbox) Push(_ context.Context, event, agentName, text string, _ reconcile.State) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pushes = append(o.pushes, event+" "+agentName)
	return nil
}
func (o *outbox) Notify(_ context.Context, text string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.alerts = append(o.alerts, text)
	return nil
}

const fleetYAML = `home: /h
hosts: {kali: {exec: [docker]}, pc2: {exec: [docker, --context, pc2]}}
orchestrator: {host: kali, token_env: O}
agents:
  agent-03: {host: kali, token_env: T3, models: [sonnet, opus]}
  agent-05: {host: pc2, token_env: T5}
`

func newFleet(t *testing.T) (*Fleet, map[string]*box, *outbox) {
	t.Helper()
	cfg, err := config.Parse([]byte(fleetYAML))
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
	for _, n := range []string{"agent-03", "agent-05", "karmax"} {
		boxes[n] = &box{transcript: map[string]string{}, obs: healthy(n)}
	}
	out := &outbox{}
	f, err := New(cfg, db, func(name string) Box { return boxes[name] }, out, out)
	if err != nil {
		t.Fatal(err)
	}
	return f, boxes, out
}

func healthy(name string) reconcile.Obs {
	return reconcile.Obs{ContainerUp: true, PaneAlive: true, PanePID: 7,
		Sessions: []observe.Agent{{PID: 7, Kind: "interactive", Name: name, SessionID: "s-" + name, Status: "idle"}}}
}

const transcript = `{"type":"user","sessionId":"s-agent-03","cwd":"/work","timestamp":"2026-10-08T10:00:00Z","message":{"role":"user","content":"go"}}
{"type":"assistant","sessionId":"s-agent-03","timestamp":"2026-10-08T10:30:00Z","message":{"id":"m","model":"claude-sonnet","content":[{"type":"text","text":"done"}],"usage":{"output_tokens":42}}}
`

func TestTickRecordsEventsQuotaAndState(t *testing.T) {
	f, boxes, _ := newFleet(t)
	st, _ := observe.ParseStatus([]byte(`{"session_id":"s-agent-03","rate_limits":{"five_hour":{"used_percentage":30,"resets_at":1791430000}}}`))
	boxes["agent-03"].obs.Status = st
	boxes["agent-03"].obs.Events = []observe.Event{{T: t0.Unix(), Event: "Stop", Session: "s-agent-03"}}

	f.Tick(context.Background(), t0)

	if boxes["agent-03"].acked != 1 {
		t.Error("events were not acknowledged after being stored")
	}
	evs, _ := f.db.Events("agent-03", time.Time{})
	if len(evs) != 1 || evs[0].Kind != "Stop" {
		t.Errorf("events = %+v", evs)
	}
	q, _ := f.db.LatestQuota()
	if q["agent-03"].FiveHour != 30 {
		t.Errorf("quota = %+v", q)
	}
	states, _ := f.db.States()
	if states["agent-03"].Phase != reconcile.Standby || states["agent-05"].SessionID != "s-agent-05" {
		t.Errorf("states = %+v", states)
	}
}

func TestDoneArchivesThenStartsFresh(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	b.transcript["s-agent-03"] = transcript
	b.wts = []agent.Worktree{
		{Path: "/work/wt/fix", Upstream: "origin/x"},
		{Path: "/work/wt/wip", Dirty: 1},
	}
	if err := f.Assign("agent-03", "T1", t0); err != nil {
		t.Fatal(err)
	}
	if err := f.Done("agent-03", "T1", "done", "fleet/agent-03/fix", "", t0); err != nil {
		t.Fatal(err)
	}
	f.Tick(context.Background(), t0)

	// Verified, then stopped (fresh), then cleaned up — never the reverse.
	want := []string{"fresh", "delete s-agent-03", "rmwt /work/wt/fix"}
	if !slices.Equal(b.calls, want) {
		t.Fatalf("calls = %q, want %q", b.calls, want)
	}
	hist, _ := f.db.History("agent-03", 5)
	if len(hist) != 1 || hist[0].Reason != reconcile.ReasonTaskDone || hist[0].Task != "T1" || hist[0].ArchiveID == "" || hist[0].Output != 42 {
		t.Fatalf("history = %+v", hist)
	}
	if st := f.State("agent-03"); st.Task != "" || st.Phase != reconcile.Standby {
		t.Errorf("state = %+v", st)
	}
	task, _ := f.db.Task("T1")
	if task.Outcome != "done" {
		t.Errorf("task = %+v", task)
	}
}

// An archive that cannot be verified stops nothing, and the decision is
// retried next tick rather than forgotten.
func TestAFailedArchiveLeavesTheSessionAlone(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	b.transcript["s-agent-03"] = transcript
	b.badHash = true
	_ = f.Assign("agent-03", "T1", t0)
	_ = f.Done("agent-03", "T1", "done", "", "", t0)

	f.Tick(context.Background(), t0)

	if len(b.calls) != 0 {
		t.Fatalf("acted on an unverified archive: %q", b.calls)
	}
	if st := f.State("agent-03"); st.Task != "T1" || !st.Done {
		t.Fatalf("the rotation was forgotten: %+v", st)
	}
	evs, _ := f.db.Events("agent-03", time.Time{})
	if len(evs) == 0 || !strings.Contains(evs[len(evs)-1].Kind, "archive_failed") {
		t.Fatalf("no record of the failure: %+v", evs)
	}
	b.badHash = false
	f.Tick(context.Background(), t0.Add(time.Minute))
	if !slices.Contains(b.calls, "fresh") {
		t.Fatalf("not retried: %q", b.calls)
	}
}

// A session that never had a turn has nothing to archive; it is simply replaced.
func TestRotatingASessionWithNoTranscript(t *testing.T) {
	f, boxes, _ := newFleet(t)
	_ = f.Assign("agent-03", "T1", t0)
	_ = f.Done("agent-03", "T1", "done", "", "", t0)
	f.Tick(context.Background(), t0)
	if b := boxes["agent-03"]; !slices.Equal(b.calls, []string{"fresh"}) {
		t.Fatalf("calls = %q", b.calls)
	}
}

func TestStraysAreArchivedThenStopped(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	b.obs.Sessions = append(b.obs.Sessions, observe.Agent{PID: 99, Kind: "interactive", Name: "helper", SessionID: "stray", Status: "idle"})
	b.transcript["stray"] = transcript
	f.Tick(context.Background(), t0)
	if !slices.Equal(b.calls, []string{"stop 99", "delete stray"}) {
		t.Fatalf("calls = %q", b.calls)
	}
}

func TestPushesAndAlertsGoOut(t *testing.T) {
	f, boxes, out := newFleet(t)
	boxes["agent-03"].obs.Events = []observe.Event{{T: t0.Unix(), Event: "StopFailure",
		Raw: json.RawMessage(`{"error_type":"authentication_failed"}`)}}
	f.Tick(context.Background(), t0)
	if !slices.Contains(out.pushes, "fleet.agent.disabled agent-03") || len(out.alerts) != 1 {
		t.Fatalf("pushes %q alerts %q", out.pushes, out.alerts)
	}
	if err := f.Enable("agent-03"); err != nil {
		t.Fatal(err)
	}
	if st := f.State("agent-03"); st.Phase == reconcile.Disabled {
		t.Fatal("enable did not clear disabled")
	}
}

func TestAssignRefusesAnUnknownOrDisabledAgent(t *testing.T) {
	f, _, _ := newFleet(t)
	if err := f.Assign("agent-99", "T", t0); err == nil {
		t.Error("assigned to an unknown agent")
	}
}

func TestStatusRows(t *testing.T) {
	f, boxes, _ := newFleet(t)
	st, _ := observe.ParseStatus([]byte(`{"context_window":{"used_percentage":33},"rate_limits":{"five_hour":{"used_percentage":85,"resets_at":1891430000},"seven_day":{"used_percentage":20}}}`))
	boxes["agent-03"].obs.Status = st
	_ = f.Assign("agent-03", "T1", t0)
	f.Tick(context.Background(), t0)
	rows := f.Status(t0.Add(10 * time.Minute))
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.Agent != "agent-03" || r.Host != "kali" || r.Task != "T1" || r.State != "waiting" || !r.Low ||
		r.FiveHour != 85 || r.SevenDay != 20 || r.Context != 33 || !slices.Equal(r.Models, []string{"sonnet", "opus"}) {
		t.Fatalf("row = %+v", r)
	}
	if r.Idle != "10m" {
		t.Errorf("idle = %q", r.Idle)
	}
}

func TestTellRelaysInTheReceiversContainer(t *testing.T) {
	f, boxes, _ := newFleet(t)
	if err := f.Tell(context.Background(), "agent-03", "agent-05", "please rebase", t0); err != nil {
		t.Fatal(err)
	}
	if got := boxes["agent-05"].calls; len(got) != 1 || got[0] != "relay agent-03→agent-05: please rebase" {
		t.Fatalf("relay calls = %q", got)
	}
	if err := f.Tell(context.Background(), "agent-05", "karmax", "done with T1", t0); err != nil {
		t.Fatal(err)
	}
	if got := boxes["karmax"].calls; len(got) != 1 {
		t.Fatalf("orchestrator relay calls = %q", got)
	}
	if err := f.Tell(context.Background(), "agent-05", "nobody", "x", t0); err == nil {
		t.Error("relayed to an unknown recipient")
	}
}

func TestRestoreArchivesTheCurrentSessionFirst(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	b.transcript["s-agent-03"] = transcript
	_ = f.Assign("agent-03", "T1", t0)
	_ = f.Done("agent-03", "T1", "done", "", "", t0)
	f.Tick(context.Background(), t0)
	hist, _ := f.db.History("agent-03", 1)
	id := hist[0].ArchiveID

	b.obs.Sessions[0].SessionID = "s-new" // the fresh session the rotation started
	b.transcript["s-new"] = strings.ReplaceAll(transcript, "s-agent-03", "s-new")
	f.Tick(context.Background(), t0.Add(time.Minute))
	b.calls = nil
	if err := f.Restore(context.Background(), id, "", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(b.calls, []string{"push s-agent-03", "restart s-agent-03", "delete s-new"}) {
		t.Fatalf("calls = %q", b.calls)
	}
	if hist, _ := f.db.History("agent-03", 5); len(hist) != 2 || hist[0].Reason != reconcile.ReasonRestored {
		t.Fatalf("history = %+v", hist)
	}
}

var _ = errors.New
