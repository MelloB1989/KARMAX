package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/agent"
	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/observe"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

const slotsYAML = `home: /h
hosts: {kali: {exec: [docker]}, pc2: {exec: [docker, --context, pc2]}}
orchestrator: {host: kali, token_env: O}
tailnet: {listen: '100.64.0.1:7878', operators: [op@example.com], members: [ram@example.com]}
slots: 2
agents:
  agent-01: {host: kali, token_env: T1}
  agent-02: {host: pc2, token_env: T2}
`

// newSlots is two agents of two slots each: four workers, all standby.
func newSlots(t *testing.T) (*Fleet, map[string]*box, *outbox) {
	t.Helper()
	cfg, err := config.Parse([]byte(slotsYAML))
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
	for _, n := range append(cfg.WorkerNames(), "karmax") {
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
	return f, boxes, out
}

func TestEveryWorkerIsTickedAndListedUnderItsAgent(t *testing.T) {
	f, boxes, _ := newSlots(t)
	boxes["agent-01:2"].obs.Events = []observe.Event{{T: t0.Unix(), Event: "UserPromptSubmit", Session: "s-agent-01:2"}}
	f.Tick(context.Background(), t0)

	var got []string
	for _, r := range f.Status(t0) {
		got = append(got, r.Agent+"/"+r.SlotOf+"/"+itoa(r.Slot)+"/"+r.Session)
	}
	want := []string{"agent-01/agent-01/1/s-agent-01", "agent-01:2/agent-01/2/s-agent-01:2", "agent-02/agent-02/1/s-agent-02", "agent-02:2/agent-02/2/s-agent-02:2"}
	if !slices.Equal(got, want) {
		t.Fatalf("roster = %v, want %v", got, want)
	}
	// Slot 2's hook events are slot 2's, on the ledger and nowhere else.
	if evs, _ := f.db.Events("agent-01:2", time.Time{}); len(evs) != 1 || evs[0].Kind != "UserPromptSubmit" {
		t.Errorf("slot 2's events = %+v", evs)
	}
	if evs, _ := f.db.Events("agent-01", time.Time{}); len(evs) != 0 {
		t.Errorf("slot 1 got slot 2's events: %+v", evs)
	}
	if boxes["agent-01:2"].acked != 1 || boxes["agent-01"].acked != 0 {
		t.Errorf("acked: slot 2 %d, slot 1 %d", boxes["agent-01:2"].acked, boxes["agent-01"].acked)
	}
}

func TestARestartOfSlot2LeavesSlot1Alone(t *testing.T) {
	f, boxes, _ := newSlots(t)
	f.Tick(context.Background(), t0)
	boxes["agent-01:2"].obs.PaneAlive = false
	boxes["agent-01:2"].obs.Sessions = nil
	f.Tick(context.Background(), t0.Add(time.Minute))

	if c := boxes["agent-01:2"].snapshotCalls(); !slices.Equal(c, []string{"restart s-agent-01:2"}) {
		t.Errorf("slot 2's calls = %v, want a restart on its own session", c)
	}
	if c := boxes["agent-01"].snapshotCalls(); len(c) != 0 {
		t.Errorf("slot 1 was touched: %v", c)
	}
	if st := f.State("agent-01"); st.SessionID != "s-agent-01" || st.Phase != reconcile.Standby {
		t.Errorf("slot 1 = %s %s", st.Phase, st.SessionID)
	}
}

// Rotating one slot archives its own session; the worktree a sibling works
// in stays, though git says it is clean and pushed.
func TestRotatingASlotKeepsTheWorktreeItsSiblingWorksIn(t *testing.T) {
	f, boxes, _ := newSlots(t)
	f.Tick(context.Background(), t0)
	b := boxes["agent-01"]
	b.transcript["s-agent-01"] = transcript
	b.wts = []agent.Worktree{{Path: "/work/wt/mine"}, {Path: "/work/wt/theirs", UsedBySlot: 2}}
	if err := f.Assign(context.Background(), "agent-01", "t1", "operator", "", t0); err != nil {
		t.Fatal(err)
	}
	if err := f.Done("agent-01", "t1", "done", "", "", t0); err != nil {
		t.Fatal(err)
	}
	f.Tick(context.Background(), t0.Add(time.Minute))

	c := b.snapshotCalls()
	if !slices.Contains(c, "fresh") || !slices.Contains(c, "rmwt /work/wt/mine") || slices.Contains(c, "rmwt /work/wt/theirs") {
		t.Errorf("slot 1's rotation: %v (want its own worktree removed, its sibling's kept)", c)
	}
	if c := boxes["agent-01:2"].snapshotCalls(); len(c) != 0 {
		t.Errorf("rotating slot 1 touched slot 2: %v", c)
	}
}

// The container is slot 1's: only it starts one Docker left stopped, and
// says it is down. Its siblings show what it shows, and wait as long.
func TestASiblingFollowsSlot1sContainerAndNeverStartsIt(t *testing.T) {
	f, boxes, out := newSlots(t)
	f.Tick(context.Background(), t0)
	for _, n := range []string{"agent-01", "agent-01:2"} {
		boxes[n].obs = reconcile.Obs{ContainerFailed: "error during container init: no such file"}
	}
	f.Tick(context.Background(), t0.Add(time.Minute))
	f.Tick(context.Background(), t0.Add(2*time.Minute))

	starts := 0
	for _, c := range boxes["agent-01"].snapshotCalls() {
		if c == "start" {
			starts++
		}
	}
	if starts == 0 {
		t.Error("slot 1 never started its container")
	}
	if c := boxes["agent-01:2"].snapshotCalls(); slices.Contains(c, "start") {
		t.Errorf("slot 2 started the container: %v", c)
	}
	one, two := f.State("agent-01"), f.State("agent-01:2")
	if two.Phase != reconcile.Down || !two.StartFailing.Equal(one.StartFailing) || one.StartFailing.IsZero() {
		t.Errorf("slot 2 = %s since %v; slot 1 = %s since %v", two.Phase, two.StartFailing, one.Phase, one.StartFailing)
	}
	if !two.Recovering(t0.Add(2 * time.Minute)) {
		t.Error("slot 2 is not recovering while slot 1 is: a wait on its task would give up")
	}
	out.mu.Lock()
	defer out.mu.Unlock()
	for _, p := range out.pushes {
		if strings.HasSuffix(p, " agent-01:2") {
			t.Errorf("slot 2 pushed %q about the container", p)
		}
	}
}

func TestCoolingAndQuotaAreTheAccountsSharedBySiblings(t *testing.T) {
	f, boxes, _ := newSlots(t)
	resets := t0.Add(3 * time.Hour)
	busy, _ := observe.ParseStatus([]byte(`{"rate_limits":{"five_hour":{"used_percentage":85,"resets_at":` + itoa(int(resets.Unix())) + `}}}`))
	stale, _ := observe.ParseStatus([]byte(`{"rate_limits":{"five_hour":{"used_percentage":40,"resets_at":` + itoa(int(resets.Unix())) + `}}}`))
	boxes["agent-01"].obs.Status = busy
	boxes["agent-01:2"].obs.Status = stale // idle since its last turn: an older reading of the same window
	f.Tick(context.Background(), t0)
	f.Tick(context.Background(), t0.Add(time.Second))
	for _, r := range f.Status(t0) {
		if r.SlotOf == "agent-01" && (r.FiveHour != 85 || !r.Low) {
			t.Errorf("%s: 5h %.0f%% low=%v, want the account's 85%% and low", r.Agent, r.FiveHour, r.Low)
		}
	}

	boxes["agent-01"].obs.Events = []observe.Event{{T: t0.Unix(), Event: "StopFailure", Session: "s-agent-01",
		Raw: []byte(`{"hook_event_name":"StopFailure","error":{"type":"rate_limit"}}`)}}
	f.Tick(context.Background(), t0.Add(time.Minute))
	boxes["agent-01"].obs.Events = nil
	f.Tick(context.Background(), t0.Add(2*time.Minute))
	one, two := f.State("agent-01"), f.State("agent-01:2")
	if one.Phase != reconcile.Cooling || two.Phase != reconcile.Cooling || !two.CoolingUntil.Equal(one.CoolingUntil) {
		t.Fatalf("slot 1 %s until %v, slot 2 %s until %v: the subscription cools as one", one.Phase, one.CoolingUntil, two.Phase, two.CoolingUntil)
	}
	if st := f.State("agent-02:2"); st.Phase == reconcile.Cooling {
		t.Error("another agent's slot cooled too")
	}
	p := f.Pick("", t0.Add(2*time.Minute))
	for _, c := range p.Candidates {
		if strings.HasPrefix(c.Agent, "agent-01") && c.Eligible {
			t.Errorf("%s is eligible while its account cools", c.Agent)
		}
	}
}

// Tasks spread over containers first: a slot of an agent that is already at
// work ranks after a slot of an idle agent, whatever its quota or idle time.
func TestPickSpreadsTasksOverAgentsBeforeSharingAContainer(t *testing.T) {
	f, _, _ := newSlots(t)
	f.Tick(context.Background(), t0)
	set(f, "agent-01:2", func(s *reconcile.State) { s.LastBusy = t0.Add(-10 * time.Hour) }) // the longest idle
	set(f, "agent-02", func(s *reconcile.State) { s.Quota.FiveHour = 50 })
	set(f, "agent-02:2", func(s *reconcile.State) { s.Quota.FiveHour = 50 })
	if err := f.Assign(context.Background(), "agent-01", "t1", "operator", "", t0); err != nil {
		t.Fatal(err)
	}
	p := f.Pick("", t0)
	if !strings.HasPrefix(p.Agent, "agent-02") {
		t.Fatalf("picked %s (%s): agent-01 is busy, agent-02 is idle", p.Agent, p.Why)
	}
	if err := f.Assign(context.Background(), p.Agent, "t2", "operator", "", t0); err != nil {
		t.Fatal(err)
	}
	// Every agent has a busy slot now: the other preferences decide.
	p = f.Pick("", t0)
	if p.Agent != "agent-01:2" || !strings.Contains(p.Why, "busy slot") {
		t.Fatalf("picked %s (%s), want agent-01:2, the longest idle", p.Agent, p.Why)
	}
}

// One container has one key and one /work for all its slots: a member's
// task and an operator-side task never share it.
func TestAMembersTaskNeverSharesAContainerWithAnOperatorsTask(t *testing.T) {
	f, _, _ := newSlots(t)
	f.Tick(context.Background(), t0)
	ctx := context.Background()
	if _, err := f.Run(ctx, RunRequest{Brief: "fix it", Agent: "agent-01", From: "ram@example.com"}, t0); err != nil {
		t.Fatal(err)
	}
	_, err := f.Run(ctx, RunRequest{Brief: "ship it", Agent: "agent-01:2", From: "operator"}, t0)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "member's task") {
		t.Fatalf("an operator's task beside a member's: %v", err)
	}
	// Nor through the pick: it goes to the other agent.
	rep, err := f.Run(ctx, RunRequest{Brief: "ship it", From: "karmax"}, t0)
	if err != nil || !strings.HasPrefix(rep.Agent, "agent-02") {
		t.Fatalf("operator side run went to %s, %v", rep.Agent, err)
	}
	// The other way round: a member is kept off agent-02's free slot.
	free := "agent-02:2"
	if rep.Agent == free {
		free = "agent-02"
	}
	_, err = f.Run(ctx, RunRequest{Brief: "help", Agent: free, From: "ram@example.com"}, t0)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "operator's task") {
		t.Fatalf("a member's task beside an operator's: %v", err)
	}
	if _, err := f.Run(ctx, RunRequest{Brief: "more", Agent: "agent-01:2", From: "ram@example.com"}, t0); err != nil {
		t.Fatalf("a member's second task beside their first: %v", err)
	}
	if f.OperatorAssigner("op@example.com") != true || f.OperatorAssigner("ram@example.com") || !f.OperatorAssigner("agent-01:2") {
		t.Error("OperatorAssigner: tailnet operator, member, worker")
	}
}

// A slot's messages are its own: it may send, and what it writes is filed
// under its task.
func TestASlotsMessagesAreFiledUnderItsOwnTask(t *testing.T) {
	f, _, _ := newSlots(t)
	f.Tick(context.Background(), t0)
	if err := f.Assign(context.Background(), "agent-01:2", "t2", "operator", "", t0); err != nil {
		t.Fatal(err)
	}
	if err := f.Tell(context.Background(), "agent-01:2", "operator", "done, branch x", t0); err != nil {
		t.Fatal(err)
	}
	msgs, err := f.db.TaskReports("t2", 0)
	if err != nil || len(msgs) != 1 || msgs[0].From != "agent-01:2" {
		t.Fatalf("t2's reports = %+v, %v", msgs, err)
	}
	if err := f.Tell(context.Background(), "agent-01:3", "operator", "hi", t0); err == nil {
		t.Error("a slot agent-01 does not have may send")
	}
}
