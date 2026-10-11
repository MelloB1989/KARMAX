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
	"github.com/MelloB1989/karmax/fleet/internal/archive"
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
	// onStop runs when the session is stopped (Fresh): a message that
	// landed between the archive and the kill.
	onStop func(b *box)
}

func (b *box) log(s string) { b.mu.Lock(); b.calls = append(b.calls, s); b.mu.Unlock() }

func (b *box) Observe(_ context.Context, now time.Time) (reconcile.Obs, error) {
	o := b.obs
	o.Now = now
	return o, nil
}
func (b *box) AckEvents(context.Context) error             { b.acked++; return nil }
func (b *box) Restart(_ context.Context, sid string) error { b.log("restart " + sid); return nil }
func (b *box) Fresh(context.Context) error {
	b.log("fresh")
	if b.onStop != nil {
		b.onStop(b)
	}
	return nil
}
func (b *box) Prompt(_ context.Context, t string) error { b.log("prompt " + t); return nil }
func (b *box) Peek(_ context.Context, n int) (string, error) {
	b.log("peek " + itoa(n))
	return "screen", nil
}
func (b *box) StopPID(_ context.Context, pid int) error { b.log("stop " + itoa(pid)); return nil }
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
	if err := f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0); err != nil {
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
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
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
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
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
	if err := f.Assign(context.Background(), "agent-99", "T", "operator", "", t0); err == nil {
		t.Error("assigned to an unknown agent")
	}
}

func TestStatusRows(t *testing.T) {
	f, boxes, _ := newFleet(t)
	st, _ := observe.ParseStatus([]byte(`{"context_window":{"used_percentage":33},"rate_limits":{"five_hour":{"used_percentage":85,"resets_at":1891430000},"seven_day":{"used_percentage":20}}}`))
	boxes["agent-03"].obs.Status = st
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
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
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
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

// A message can land in the old session after it was archived and before it
// was stopped. It is archived too — never deleted unarchived — and the first
// archive, now a strict prefix of the second, is not kept beside it.
func TestAMessageAfterTheArchiveIsArchivedBeforeDeletion(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	b.transcript["s-agent-03"] = transcript
	late := `{"type":"user","sessionId":"s-agent-03","timestamp":"2026-10-08T10:31:00Z","turnOrigin":"peer","message":{"role":"user","content":"next task: T2"}}` + "\n"
	b.onStop = func(b *box) { b.transcript["s-agent-03"] += late; b.onStop = nil }
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	_ = f.Done("agent-03", "T1", "done", "", "", t0)
	f.Tick(context.Background(), t0)

	all, _ := archive.List(f.ArchiveRoot(), time.Time{})
	if len(all) != 1 {
		t.Fatalf("archives = %d, want only the one with the late message", len(all))
	}
	if hist, _ := f.db.History("agent-03", 5); len(hist) != 1 || hist[0].ArchiveID != all[0].ID {
		t.Fatalf("history points at %+v, want %s", hist, all[0].ID)
	}
	found := false
	for _, m := range all {
		if tr, err := archive.ReadTranscript(f.ArchiveRoot(), m.ID); err == nil && strings.Contains(string(tr.Main), "next task: T2") {
			found = true
		}
	}
	if !found {
		t.Fatal("the late message is in no archive")
	}
	if !slices.Contains(b.calls, "delete s-agent-03") {
		t.Fatalf("calls = %q", b.calls)
	}
}

func TestALateMessageThatCannotBeArchivedKeepsTheTranscript(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	b.transcript["s-agent-03"] = transcript
	b.onStop = func(b *box) { b.transcript["s-agent-03"] += "{}\n"; b.badHash = true }
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	_ = f.Done("agent-03", "T1", "done", "", "", t0)
	f.Tick(context.Background(), t0)
	if slices.Contains(b.calls, "delete s-agent-03") {
		t.Fatalf("deleted a transcript holding unarchived messages: %q", b.calls)
	}
}

// A relayed message says who sent it, and the receiver replies there: the
// sender has to be someone the fleet can deliver a reply to.
func TestTellNeedsAKnownSender(t *testing.T) {
	f, _, _ := newFleet(t)
	if err := f.Tell(context.Background(), "mallory", "agent-03", "x", t0); err == nil {
		t.Error("relayed from an unknown sender")
	}
	for _, from := range []string{"agent-05", "karmax", "operator"} {
		if err := f.Tell(context.Background(), from, "agent-03", "x", t0); err != nil {
			t.Errorf("from %s: %v", from, err)
		}
	}
}

// Restoring the session that is already live would overwrite its transcript
// with an older copy and silently drop every turn since; restoring onto an
// agent mid-turn would kill that turn. Both are refused.
func TestRestoreRefusesTheLiveSessionAndABusyAgent(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	b.transcript["s-agent-03"] = transcript
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	_ = f.Done("agent-03", "T1", "done", "", "", t0)
	f.Tick(context.Background(), t0)
	hist, _ := f.db.History("agent-03", 1)
	id := hist[0].ArchiveID

	// The old session is still what the pane runs (agent-run not simulated).
	f.Tick(context.Background(), t0.Add(time.Minute))
	b.calls = nil
	if err := f.Restore(context.Background(), id, "", t0.Add(time.Hour)); err == nil {
		t.Fatal("restored over the live session it came from")
	}
	if len(b.calls) != 0 {
		t.Fatalf("acted anyway: %q", b.calls)
	}

	b.obs.Sessions[0].SessionID, b.obs.Sessions[0].Status = "s-new", "busy"
	b.transcript["s-new"] = strings.ReplaceAll(transcript, "s-agent-03", "s-new")
	f.Tick(context.Background(), t0.Add(2*time.Minute))
	b.calls = nil
	if err := f.Restore(context.Background(), id, "", t0.Add(time.Hour)); err == nil || len(b.calls) != 0 {
		t.Fatalf("restored onto an agent mid-turn: %v %q", err, b.calls)
	}
}

func TestTellToOperatorIsStoredNotRelayed(t *testing.T) {
	f, boxes, _ := newFleet(t)
	if err := f.Tell(context.Background(), "agent-03", "operator", "ready on fleet/agent-03/x", t0); err != nil {
		t.Fatal(err)
	}
	for name, b := range boxes {
		if len(b.calls) != 0 {
			t.Fatalf("%s was called: %v", name, b.calls)
		}
	}
	msgs, err := f.Inbox(0)
	if err != nil || len(msgs) != 1 || msgs[0].From != "agent-03" || msgs[0].Text != "ready on fleet/agent-03/x" || msgs[0].ID == 0 {
		t.Fatalf("inbox = %+v, %v", msgs, err)
	}
	if later, _ := f.Inbox(msgs[0].ID); len(later) != 0 {
		t.Fatalf("inbox after its last id = %+v", later)
	}
}

func TestAnUnknownSenderCannotWriteToTheInbox(t *testing.T) {
	f, _, _ := newFleet(t)
	if err := f.Tell(context.Background(), "agent-99", "operator", "hi", t0); err == nil {
		t.Fatal("want an error")
	}
}

func TestAssignWithBriefRecordsAssignerAndDeliversTheBrief(t *testing.T) {
	f, boxes, _ := newFleet(t)
	if err := f.Assign(context.Background(), "agent-05", "T7", "operator", "build the thing", t0); err != nil {
		t.Fatal(err)
	}
	task, err := f.DB().Task("T7")
	if err != nil || task.Assigner != "operator" || task.Agent != "agent-05" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if want := "relay operator→agent-05: build the thing"; !slices.Contains(boxes["agent-05"].calls, want) {
		t.Fatalf("calls = %v, want %q", boxes["agent-05"].calls, want)
	}
}

func TestAssignWithoutBriefDeliversNothing(t *testing.T) {
	f, boxes, _ := newFleet(t)
	_ = f.Assign(context.Background(), "agent-05", "T8", "karmax", "", t0)
	if len(boxes["agent-05"].calls) != 0 {
		t.Fatalf("calls = %v", boxes["agent-05"].calls)
	}
}

func waitAsync(f *Fleet, ctx context.Context, agent, task string, after int64) chan WaitResult {
	done := make(chan WaitResult, 1)
	go func() {
		r, err := f.Wait(ctx, agent, task, after)
		if err != nil {
			r.Reason = "error: " + err.Error()
		}
		done <- r
	}()
	return done
}

func await(t *testing.T, done chan WaitResult) WaitResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return")
		return WaitResult{}
	}
}

func TestWaitReturnsOnTheAgentsMessageToTheOperator(t *testing.T) {
	f, _, _ := newFleet(t)
	f.waitEvery = 10 * time.Millisecond
	_ = f.Tell(context.Background(), "agent-05", "operator", "old news", t0)
	old, _ := f.Inbox(0)
	done := waitAsync(f, context.Background(), "agent-03", "", old[0].ID)
	time.Sleep(30 * time.Millisecond)
	_ = f.Tell(context.Background(), "agent-05", "operator", "not mine", t0)
	_ = f.Tell(context.Background(), "agent-03", "operator", "ready", t0)
	r := await(t, done)
	if r.Reason != "message" || r.Message == nil || r.Message.Text != "ready" {
		t.Fatalf("wait = %+v", r)
	}
}

func TestWaitReturnsWhenTheTaskCloses(t *testing.T) {
	f, _, _ := newFleet(t)
	f.waitEvery = 10 * time.Millisecond
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	done := waitAsync(f, context.Background(), "agent-03", "T1", 0)
	time.Sleep(30 * time.Millisecond)
	_ = f.Done("agent-03", "T1", "done", "fleet/agent-03/x", "", t0)
	if r := await(t, done); r.Reason != "closed" {
		t.Fatalf("wait = %+v", r)
	}
}

func TestWaitReturnsOnDownCoolingOrDisabled(t *testing.T) {
	for _, p := range []reconcile.Phase{reconcile.Down, reconcile.Cooling, reconcile.Disabled} {
		f, _, _ := newFleet(t)
		f.waitEvery = 10 * time.Millisecond
		done := waitAsync(f, context.Background(), "agent-03", "", 0)
		time.Sleep(30 * time.Millisecond)
		st := f.State("agent-03")
		st.Phase = p
		f.setState(st)
		if r := await(t, done); r.Reason != "phase" || r.Phase != string(p) {
			t.Fatalf("%s: wait = %+v", p, r)
		}
	}
}

func TestWaitTimesOutWithoutAnError(t *testing.T) {
	f, _, _ := newFleet(t)
	f.waitEvery = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if r := await(t, waitAsync(f, ctx, "agent-03", "", 0)); r.Reason != "timeout" {
		t.Fatalf("wait = %+v", r)
	}
}

func TestWaitOnAnUnknownAgentFails(t *testing.T) {
	f, _, _ := newFleet(t)
	if _, err := f.Wait(context.Background(), "agent-99", "", 0); err == nil {
		t.Fatal("want an error")
	}
}

func TestAssignSupersedesTheAgentsOpenTasksWithoutRotating(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	ctx := context.Background()
	// Two stale open tasks (only the latest is the agent's current one), and
	// another agent's task that must not be touched.
	_ = f.Assign(ctx, "agent-03", "OLD1", "operator", "", t0)
	_ = f.Assign(ctx, "agent-03", "OLD2", "operator", "", t0.Add(time.Minute))
	_ = f.Assign(ctx, "agent-05", "OTHER", "operator", "", t0)
	if task, _ := f.db.Task("OLD1"); task.Outcome != "superseded" || task.Closed.IsZero() {
		t.Fatalf("OLD1 after OLD2 was assigned: %+v", task)
	}
	if err := f.Assign(ctx, "agent-03", "NEW", "operator", "go", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"OLD1", "OLD2"} {
		if task, _ := f.db.Task(id); task.Outcome != "superseded" || task.Closed.IsZero() {
			t.Errorf("%s = %+v, want closed as superseded", id, task)
		}
	}
	if task, _ := f.db.Task("NEW"); !task.Closed.IsZero() || task.Outcome != "" {
		t.Errorf("NEW = %+v, want open", task)
	}
	if task, _ := f.db.Task("OTHER"); !task.Closed.IsZero() {
		t.Errorf("another agent's task was closed: %+v", task)
	}
	// Assigning the same id again does not supersede itself.
	_ = f.Assign(ctx, "agent-03", "NEW", "operator", "", t0.Add(3*time.Minute))
	if task, _ := f.db.Task("NEW"); !task.Closed.IsZero() {
		t.Errorf("a re-assigned task closed itself: %+v", task)
	}
	if st := f.State("agent-03"); st.Task != "NEW" || st.Done {
		t.Errorf("state = %+v", st)
	}
	// The session goes on: nothing archived, nothing rotated.
	f.Tick(ctx, t0.Add(4*time.Minute))
	for _, c := range b.calls {
		if strings.HasPrefix(c, "fresh") || strings.HasPrefix(c, "delete") {
			t.Errorf("assign rotated the session: %q", b.calls)
		}
	}
	if hist, _ := f.db.History("agent-03", 5); len(hist) != 0 {
		t.Errorf("history = %+v, want none", hist)
	}
	evs, _ := f.db.Events("agent-03", time.Time{})
	n := 0
	for _, e := range evs {
		if e.Kind == "fleet.superseded" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("fleet.superseded events = %d, want 2 (OLD1 by OLD2, OLD2 by NEW)", n)
	}
}

func TestCloseKeepsTheSessionAndTheCurrentTaskGoesAway(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	ctx := context.Background()
	_ = f.Assign(ctx, "agent-03", "T1", "operator", "", t0)
	if err := f.Close("agent-03", "", "done", "fleet/agent-03/x", "", t0); err != nil {
		t.Fatal(err)
	}
	task, _ := f.db.Task("T1")
	if task.Outcome != "done" || task.Branch != "fleet/agent-03/x" || task.Closed.IsZero() {
		t.Errorf("task = %+v", task)
	}
	if st := f.State("agent-03"); st.Task != "" || st.Done || st.Phase == reconcile.Waiting {
		t.Errorf("state = %+v, want no task, not waiting, and not marked done (that is what rotates)", st)
	}
	f.Tick(ctx, t0.Add(time.Minute))
	if len(b.calls) != 0 {
		t.Errorf("calls = %q, want none: the session was kept", b.calls)
	}
	if hist, _ := f.db.History("agent-03", 5); len(hist) != 0 {
		t.Errorf("history = %+v", hist)
	}
	// Nothing to close.
	if err := f.Close("agent-03", "", "", "", "", t0); err == nil {
		t.Error("closing with no task succeeded")
	}
	// A stale task other than the current one: closed, current untouched.
	_ = f.Assign(ctx, "agent-05", "S1", "operator", "", t0)
	f.db.OpenTask("S0", "agent-05", "operator", t0)
	if err := f.Close("agent-05", "S0", "superseded", "", "", t0); err != nil {
		t.Fatal(err)
	}
	if st := f.State("agent-05"); st.Task != "S1" {
		t.Errorf("current task = %q, want S1 untouched", st.Task)
	}
	// And plain Done still rotates (one task, one session).
	_ = f.Assign(ctx, "agent-03", "T2", "operator", "", t0)
	_ = f.Done("agent-03", "T2", "done", "", "", t0)
	if st := f.State("agent-03"); !st.Done {
		t.Error("Done no longer marks the session for rotation")
	}
}

// Friction #4: `fleetctl done` showed in status only at the next tick.
func TestDoneShowsInStatusAtOnce(t *testing.T) {
	f, boxes, _ := newFleet(t)
	boxes["agent-03"].transcript["s-agent-03"] = transcript
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	f.Tick(context.Background(), t0) // the agent is idle on its task: waiting
	row := func() Row {
		for _, r := range f.Status(t0) {
			if r.Agent == "agent-03" {
				return r
			}
		}
		t.Fatal("no row")
		return Row{}
	}
	if r := row(); r.Task != "T1" || r.State != string(reconcile.Waiting) {
		t.Fatalf("before done: %+v", r)
	}

	if err := f.Done("agent-03", "T1", "done", "", "", t0); err != nil {
		t.Fatal(err)
	}
	// no tick in between
	if r := row(); r.Task != "" || r.State != string(reconcile.Standby) {
		t.Fatalf("right after done: task %q state %q, want none / standby", r.Task, r.State)
	}
	if tk, err := f.DB().Task("T1"); err != nil || tk.Closed.IsZero() || tk.Outcome != "done" {
		t.Fatalf("task = %+v %v", tk, err)
	}
	// wait sees it at once too
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if w, err := f.Wait(ctx, "agent-03", "T1", 0); err != nil || w.Reason != "closed" {
		t.Fatalf("wait = %+v %v", w, err)
	}
	// the rotation still waits for the tick, and still knows the task
	if st := f.State("agent-03"); st.Task != "T1" || !st.Done {
		t.Fatalf("the pending rotation lost its task: %+v", st)
	}
	f.Tick(context.Background(), t0.Add(time.Minute))
	if r := row(); r.Task != "" || r.State != string(reconcile.Standby) {
		t.Fatalf("after the rotation tick: %+v", r)
	}
}
