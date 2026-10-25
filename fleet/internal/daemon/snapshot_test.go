package daemon

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

var guest = fleetapi.Me{Name: "friend@example.com", Role: fleetapi.RoleGuest, Device: "friend-laptop.tail1234.ts.net", Lent: []string{"agent-03"}}

func TestASnapshotForAGuestHoldsOnlyItsAgentsAndTasks(t *testing.T) {
	f, _, _ := newFleet(t)
	ctx := context.Background()
	// Another guest's task on the same agent, closed when the friend's came.
	if err := f.Assign(ctx, "agent-03", "T-pal", "pal@example.com", "", t0); err != nil {
		t.Fatal(err)
	}
	if err := f.Assign(ctx, "agent-03", "T-friend", "friend@example.com/laptop", "", t0); err != nil {
		t.Fatal(err)
	}
	if err := f.Assign(ctx, "agent-05", "T-op", "operator", "", t0); err != nil {
		t.Fatal(err)
	}
	f.event("agent-05", "fleet.alert", "", map[string]string{"text": "agent-05 is low"})
	f.event("agent-03", "fleet.prompt", "", map[string]string{"text": "carry on"})
	for _, acct := range []string{"agent-03", "agent-05", "karmax"} {
		if err := f.db.AddQuota(ledger.Quota{Account: acct, At: t0, FiveHour: 30, FiveResets: t0.Add(2 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	f.containerSamples([]ledger.HostSample{{Container: "agent-03", At: t0, CPU: 12, MemBytes: 1 << 30},
		{Container: "agent-05", At: t0, CPU: 50, MemBytes: 2 << 30}})
	f.hostExec = func(context.Context, string, string, ...string) ([]byte, error) { return []byte(hostOut), nil }
	f.sampleHost(ctx, "kali", t0)
	f.sampleHost(ctx, "pc2", t0)

	now := t0.Add(time.Minute)
	all := f.Snapshot(now, true, nil, fleetapi.Me{Name: "operator", Role: fleetapi.RoleAdmin})
	if len(all.Workers) != 2 || len(all.Tasks) != 3 || len(all.Quota) != 3 || len(all.Hosts) != 2 {
		t.Fatalf("an admin sees everything: workers %d, tasks %d, quota %d, hosts %d", len(all.Workers), len(all.Tasks), len(all.Quota), len(all.Hosts))
	}

	s := f.Snapshot(now, false, []string{"agent-03"}, guest)
	if s.Me.Name != guest.Name || s.Me.Role != fleetapi.RoleGuest {
		t.Errorf("me = %+v", s.Me)
	}
	if len(s.Workers) != 1 || s.Workers[0].Agent != "agent-03" || s.Workers[0].Task != "T-friend" {
		t.Errorf("workers = %+v", s.Workers)
	}
	if len(s.Tasks) != 1 || s.Tasks[0].ID != "T-friend" {
		t.Errorf("tasks = %+v", s.Tasks)
	}
	if len(s.Events) == 0 {
		t.Error("the guest sees none of its agent's events")
	}
	for _, e := range s.Events {
		if agentOf(e.Agent) != "agent-03" || strings.Contains(string(e.Detail), "T-pal") || strings.Contains(string(e.Detail), "T-op") {
			t.Errorf("the guest sees %s %s %s", e.Agent, e.Kind, e.Detail)
		}
	}
	if len(s.Quota) != 1 || s.Quota[0].Name != "agent-03" || !slices.Equal(s.Quota[0].Agents, []string{"agent-03"}) {
		t.Errorf("quota = %+v", s.Quota)
	}
	if len(s.Hosts) != 1 || s.Hosts[0].Name != "kali" || len(s.Hosts[0].Containers) != 1 || s.Hosts[0].Containers[0].Agent != "agent-03" {
		t.Fatalf("hosts = %+v", s.Hosts)
	}
	if h := s.Hosts[0]; h.MemTotal != 0 || h.Load != [3]float64{} || len(h.Load1) != 0 || h.DiskTotal != 0 {
		t.Errorf("the guest sees the host's own numbers: %+v", h)
	}
	if s.Knocks != 0 || len(s.Fleetd.TickErrors) != 0 {
		t.Errorf("knocks %d, tick errors %v", s.Knocks, s.Fleetd.TickErrors)
	}
	// What the guest gets is its own: cutting it leaves the shared whole alone.
	again := f.Snapshot(now, true, nil, fleetapi.Me{Name: "operator", Role: fleetapi.RoleAdmin})
	if len(again.Workers) != 2 || again.Workers[0].Task == "" && again.Workers[1].Task == "" {
		t.Errorf("the whole changed under the guest's cut: %+v", again.Workers)
	}
}

func TestAGuestDoesNotSeeAnotherGuestsTaskOnItsLentAgent(t *testing.T) {
	f, _, _ := newFleet(t)
	if err := f.Assign(context.Background(), "agent-03", "T-pal", "pal@example.com", "", t0); err != nil {
		t.Fatal(err)
	}
	s := f.Snapshot(t0, false, []string{"agent-03"}, guest)
	if len(s.Workers) != 1 || s.Workers[0].Task != "" || len(s.Tasks) != 0 {
		t.Fatalf("workers %+v, tasks %+v", s.Workers, s.Tasks)
	}
	for _, e := range s.Events {
		if strings.Contains(string(e.Detail), "T-pal") {
			t.Errorf("event %s %s", e.Kind, e.Detail)
		}
	}
}

func TestAGuestWithNoLendsSeesNothing(t *testing.T) {
	f, _, _ := newFleet(t)
	if err := f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0); err != nil {
		t.Fatal(err)
	}
	s := f.Snapshot(t0, false, nil, guest)
	if len(s.Workers)+len(s.Tasks)+len(s.Events)+len(s.Quota)+len(s.Hosts) != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "null") {
		t.Errorf("empty lists are null: %s", b)
	}
}

func TestBurnAndRunOut(t *testing.T) {
	f, _, _ := newFleet(t)
	now := t0
	resets := now.Add(2 * time.Hour)
	// agent-03 grows 10 %/h and agent-05 40 %/h, both at 40 % now.
	for m := 60; m >= 0; m -= 5 {
		at := now.Add(-time.Duration(m) * time.Minute)
		for acct, rate := range map[string]float64{"agent-03": 10, "agent-05": 40} {
			if err := f.db.AddQuota(ledger.Quota{Account: acct, At: at, FiveHour: 40 - rate*float64(m)/60, FiveResets: resets}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A sample of the window before, which must not bend the fit.
	_ = f.db.AddQuota(ledger.Quota{Account: "agent-05", At: now.Add(-50 * time.Minute), FiveHour: 99, FiveResets: now.Add(-40 * time.Minute)})
	s := f.Snapshot(now, true, nil, fleetapi.Me{})
	acct := map[string]fleetapi.Account{}
	for _, a := range s.Quota {
		acct[a.Name] = a
	}
	slow, fast := acct["agent-03"], acct["agent-05"]
	if math.Abs(slow.BurnPerHour-10) > 0.01 || !slow.RunOut.IsZero() {
		t.Errorf("10 %%/h: burn %.2f, run-out %v (the window resets first)", slow.BurnPerHour, slow.RunOut)
	}
	if math.Abs(fast.BurnPerHour-40) > 0.01 {
		t.Errorf("40 %%/h: burn %.2f", fast.BurnPerHour)
	}
	if d := fast.RunOut.Sub(now.Add(90 * time.Minute)); d < -time.Minute || d > time.Minute {
		t.Errorf("40 %%/h: run-out %v, want %v", fast.RunOut, now.Add(90*time.Minute))
	}
	if len(slow.History) != 13 || slow.History[12].V != 40 {
		t.Errorf("history = %+v", slow.History)
	}
	if last := s.Quota[len(s.Quota)-1]; last.Name != "karmax" || !last.Orchestrator {
		t.Errorf("the orchestrator's account is not last: %+v", last)
	}
}

func TestTheTaskListHasItsLastMessage(t *testing.T) {
	f, _, _ := newFleet(t)
	if err := f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddRelay(ledger.Relay{From: "operator", To: "agent-03", Task: "T1", Text: "the brief", At: t0, Delivered: true}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("é", 150) // 300 bytes, cut on a rune
	if err := f.TellKind(context.Background(), "agent-03", "operator", "Which one?\n"+long, ledger.KindQuestion, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddRelay(ledger.Relay{From: "agent-03", To: "operator", Task: "T1", Text: "  \n" + long + "\nmore", At: t0.Add(2 * time.Minute), Delivered: true}); err != nil {
		t.Fatal(err)
	}
	s := f.Snapshot(t0.Add(3*time.Minute), true, nil, fleetapi.Me{})
	if len(s.Tasks) != 1 {
		t.Fatalf("tasks = %+v", s.Tasks)
	}
	got := s.Tasks[0]
	if got.Messages != 3 || got.LastKind != "report" || !got.LastAt.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("task = %+v", got)
	}
	if len(got.Last) > 200 || !strings.HasPrefix(long, got.Last) || len(got.Last) < 198 {
		t.Errorf("last = %q (%d bytes)", got.Last, len(got.Last))
	}
}

func TestATaskClosedADayAgoLeavesTheList(t *testing.T) {
	f, _, _ := newFleet(t)
	for _, id := range []string{"T-old", "T-new"} {
		if err := f.db.OpenTask(id, "agent-03", "operator", t0.Add(-30*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.db.CloseTask("T-old", t0.Add(-25*time.Hour), "done", "", "")
	_ = f.db.CloseTask("T-new", t0.Add(-23*time.Hour), "done", "", "")
	s := f.Snapshot(t0, true, nil, fleetapi.Me{})
	if len(s.Tasks) != 1 || s.Tasks[0].ID != "T-new" || s.Tasks[0].Messages != 0 {
		t.Fatalf("tasks = %+v", s.Tasks)
	}
}

const hostOut = `0.52 0.61 0.70 2/1234 5678
MemTotal:       16000000 kB
MemAvailable:    6000000 kB
8
/dev/nvme0n1p2   1000000 250000 750000      25% /home/op/code
`

func TestHostSamplesKeepSixty(t *testing.T) {
	f, _, _ := newFleet(t)
	var mu sync.Mutex
	var calls []string
	f.hostExec = func(_ context.Context, worker, _ string, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, worker+" "+strings.Join(args, " "))
		mu.Unlock()
		return []byte(hostOut), nil
	}
	for i := range 70 {
		f.sampleHost(context.Background(), "kali", t0.Add(time.Duration(i)*time.Minute))
		f.containerSamples([]ledger.HostSample{{Container: "agent-03", At: t0.Add(time.Duration(i) * time.Minute), CPU: float64(i)}})
	}
	if len(calls) != 70 || calls[0] != "agent-03 /h/code" {
		t.Fatalf("execs = %d, first %q", len(calls), calls[0])
	}
	var kali fleetapi.Host
	for _, h := range f.Snapshot(t0.Add(70*time.Minute), true, nil, fleetapi.Me{}).Hosts {
		if h.Name == "kali" {
			kali = h
		}
	}
	if len(kali.Load1) != 60 || !kali.Load1[0].T.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("load history = %d points from %v", len(kali.Load1), kali.Load1[0].T)
	}
	if kali.CPUs != 8 || kali.Load != [3]float64{0.52, 0.61, 0.70} || kali.MemTotal != 16000000<<10 ||
		kali.MemUsed != 10000000<<10 || kali.DiskTotal != 1000000<<10 || kali.DiskUsed != 250000<<10 || !kali.Reachable {
		t.Errorf("host = %+v", kali)
	}
	c := kali.Containers[0]
	if len(c.CPUHist) != 60 || c.CPU != 69 || c.CPUHist[0].V != 10 {
		t.Errorf("container = %d samples, cpu %v, first %v", len(c.CPUHist), c.CPU, c.CPUHist[0])
	}
}

func TestHostsAreAskedOnceAMinuteWithOneExecEach(t *testing.T) {
	f, _, _ := newFleet(t)
	var mu sync.Mutex
	execs := map[string]int{}
	f.hostExec = func(_ context.Context, worker, _ string, _ ...string) ([]byte, error) {
		mu.Lock()
		execs[worker]++
		mu.Unlock()
		return []byte(hostOut), nil
	}
	for _, at := range []time.Duration{0, 20 * time.Second, 40 * time.Second, 61 * time.Second} {
		f.Tick(context.Background(), t0.Add(at))
	}
	mu.Lock()
	defer mu.Unlock()
	if execs["agent-03"] != 2 || execs["agent-05"] != 2 || len(execs) != 2 {
		t.Fatalf("execs = %v, want two per host over four ticks a minute apart", execs)
	}
}

func TestAHostWhoseAgentsAreDownIsNotAsked(t *testing.T) {
	f, _, _ := newFleet(t)
	f.setState(reconcile.State{Name: "agent-05", Phase: reconcile.Down})
	asked := 0
	f.hostExec = func(context.Context, string, string, ...string) ([]byte, error) { asked++; return []byte(hostOut), nil }
	f.sampleHost(context.Background(), "pc2", t0)
	if asked != 0 {
		t.Fatalf("asked a host with nothing running %d times", asked)
	}
}

func TestATickRecordsItsDurationAndErrors(t *testing.T) {
	f, boxes, _ := newFleet(t)
	boxes["agent-05"].observeErr = context.DeadlineExceeded
	f.hostExec = func(context.Context, string, string, ...string) ([]byte, error) { return []byte("nonsense"), nil }
	before := f.SnapshotSeq()
	f.Tick(context.Background(), t0)
	<-f.Changed() // the tick's end moves the stream, within the second
	s := f.Snapshot(t0, true, nil, fleetapi.Me{})
	if s.Seq <= before || !s.Fleetd.TickAt.Equal(t0) || s.Fleetd.TickMS < 0 {
		t.Errorf("seq %d (was %d), fleetd %+v", s.Seq, before, s.Fleetd)
	}
	joined := strings.Join(s.Fleetd.TickErrors, "; ")
	if !strings.Contains(joined, "pc2 does not answer") || !strings.Contains(joined, "unreadable host sample") {
		t.Errorf("tick errors = %v", s.Fleetd.TickErrors)
	}
}

func TestATelemetryToolEventBecomesTheWorkersActivity(t *testing.T) {
	f, _, _ := newFleet(t)
	f.setState(reconcile.State{Name: "agent-03", Phase: reconcile.Working, SessionID: "s-agent-03"})
	now := time.Now()
	rec := f.Recorder()
	for _, e := range []ledger.Event{
		{Agent: "agent-03", At: now.Add(-2 * time.Second), Kind: "otel.tool_decision", Session: "s-agent-03", Detail: `{"tool_name":"Bash","command":"go test ./..."}`},
		{Agent: "agent-03", At: now.Add(-time.Second), Kind: "otel.tool_result", Session: "s-agent-03", Detail: `{"tool_name":"Bash","command":"git push"}`},
		{Agent: "agent-03", At: now.Add(-time.Minute), Kind: "otel.tool_result", Session: "s-agent-03", Detail: `{"tool_name":"Read"}`}, // late, and older
		{Agent: "agent-03", At: now, Kind: "otel.tool_result", Session: "s-other", Detail: `{"tool_name":"Edit"}`},                      // another session's
	} {
		if err := rec.AddEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	s := f.Snapshot(now, true, nil, fleetapi.Me{})
	var w fleetapi.Worker
	for _, x := range s.Workers {
		if x.Agent == "agent-03" {
			w = x
		}
	}
	if w.Activity.Kind != fleetapi.ActPushing || w.Activity.Tool != "Bash" {
		t.Fatalf("activity = %+v", w.Activity)
	}
	for _, e := range s.Events {
		if e.Kind == "otel.tool_decision" {
			t.Errorf("a tool decision is in the event list: %+v", e)
		}
	}
}

func TestTheStreamMovesAtMostOnceASecond(t *testing.T) {
	f, _, _ := newFleet(t)
	first := f.Changed()
	f.touch() // whatever New and the setup did is behind us
	<-first
	ch, seq := f.Changed(), f.SnapshotSeq()
	start := time.Now()
	f.touch()
	f.touch()
	f.touch()
	<-ch
	if f.SnapshotSeq() != seq+1 {
		t.Fatalf("seq = %d, want %d: three changes in a second are one snapshot", f.SnapshotSeq(), seq+1)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Errorf("the next snapshot came %v after the last", time.Since(start))
	}
}
