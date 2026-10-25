package fleetapi

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// Fake is a made-up fleet: the TUI's tests and `fleetctl tui --demo` run on
// it. It is deterministic for a given clock and number of Steps. Watch
// blocks until Step makes a newer snapshot; actions are recorded in Calls
// and change the fleet the way fleetd would, roughly.
type Fake struct {
	Role Role
	Now  func() time.Time

	mu      sync.Mutex
	step    int
	seq     uint64
	changed chan struct{}
	tasks   []Task
	devices []Device
	knocks  []Knock
	Calls   []string
}

// NewFake is a fleet of twelve agents as an admin (or a guest lent agent-09)
// sees it, at now.
func NewFake(role Role, now func() time.Time) *Fake {
	f := &Fake{Role: role, Now: now, seq: 1, changed: make(chan struct{})}
	t := now()
	f.tasks = []Task{
		{ID: "20261025-tui-frame-1a2b", Agent: "agent-01", Assigner: "operator", Repo: "KARMAX", Assigned: t.Add(-42 * time.Minute), AdminWork: true,
			Messages: 3, Last: "frame and tabs render at 80x24; golden files added", LastAt: t.Add(-3 * time.Minute), LastKind: "progress"},
		{ID: "20261025-access-3c4d", Agent: "agent-04", Assigner: "operator", Repo: "KARMAX", Assigned: t.Add(-38 * time.Minute), AdminWork: true,
			Messages: 2, Last: "Should a lend to an admin device be refused or turn it into a guest?", LastAt: t.Add(-1 * time.Minute), LastKind: "question"},
		{ID: "20261025-term-5e6f", Agent: "agent-05:2", Assigner: "operator", Repo: "KARMAX", Assigned: t.Add(-35 * time.Minute), AdminWork: true,
			Messages: 1, Last: "on it", LastAt: t.Add(-34 * time.Minute), LastKind: "report"},
		{ID: "20261025-port-api-7a8b", Agent: "agent-09", Assigner: "friend@example.com", Repo: "", Assigned: t.Add(-12 * time.Minute),
			Messages: 1, Last: "synced your repo into /work/code/api; running its tests", LastAt: t.Add(-6 * time.Minute), LastKind: "progress"},
		{ID: "20261025-fix-login-9c0d", Agent: "agent-06", Assigner: "operator", Repo: "KARMAX", Assigned: t.Add(-3 * time.Hour), Closed: t.Add(-2 * time.Hour),
			Outcome: "done", Branch: "fleet/agent-06/fix-login", AdminWork: true, Messages: 4, Last: "merged-ready: branch fleet/agent-06/fix-login", LastAt: t.Add(-2 * time.Hour), LastKind: "report"},
	}
	f.devices = []Device{
		{StableID: "nST1", Name: "your-mac.tail1234.ts.net", Login: "you@example.com", Role: RoleAdmin, TaggedAt: t.Add(-48 * time.Hour), TaggedBy: "operator", LastSeen: t.Add(-time.Minute)},
		{StableID: "nST2", Name: "friend-laptop.tail5678.ts.net", Login: "friend@example.com", Role: RoleGuest, Agents: []string{"agent-09"}, TaggedAt: t.Add(-5 * time.Hour), TaggedBy: "your-mac", LastSeen: t.Add(-6 * time.Minute)},
	}
	f.knocks = []Knock{{StableID: "nST3", Name: "pal-desktop.tail9abc.ts.net", Login: "pal@example.com", First: t.Add(-20 * time.Minute), Last: t.Add(-4 * time.Minute), Count: 3}}
	return f
}

// Step moves the fake fleet on: activities turn, quota grows, and a watcher
// gets the new snapshot.
func (f *Fake) Step() {
	f.mu.Lock()
	f.step++
	f.seq++
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

func (f *Fake) Watch(ctx context.Context, since uint64) (*Snapshot, error) {
	for {
		f.mu.Lock()
		if since < f.seq {
			s := f.snapshot()
			f.mu.Unlock()
			return s, nil
		}
		ch := f.changed
		f.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

var fakeActs = []ActivityKind{ActReading, ActEditing, ActRunning, ActTesting, ActThinking, ActEditing, ActPushing}
var fakeTools = map[ActivityKind]string{ActReading: "Grep", ActEditing: "Edit", ActRunning: "Bash", ActTesting: "Bash", ActPushing: "Bash"}

func (f *Fake) snapshot() *Snapshot {
	now := f.Now()
	s := &Snapshot{Seq: f.seq, At: now, Today: Today{Since: midnight(now), TasksClosed: 7, SpendUSD: 41.37, OutputTokens: 1_284_000},
		Fleetd: Fleetd{Version: "v0.3.0-demo", Started: now.Add(-26 * time.Hour), TickAt: now.Add(-4 * time.Second), TickMS: 2140}}
	open := map[string]string{} // worker → task
	for _, t := range f.tasks {
		if t.Closed.IsZero() {
			open[t.Agent] = t.ID
		}
	}
	type spec struct {
		agent, host, account string
		slots                int
		sandbox              bool
		five                 float64
		state                string
	}
	agents := []spec{
		{"agent-01", "pc2", "acct-01", 2, false, 34, ""}, {"agent-02", "pc2", "acct-02", 2, false, 99, ""},
		{"agent-03", "pc2", "acct-03", 2, false, 12, "cooling"}, {"agent-04", "pc2", "acct-04", 2, false, 51, ""},
		{"agent-05", "pc2", "acct-05", 2, false, 43, ""}, {"agent-06", "pc2", "acct-06", 2, false, 8, ""},
		{"agent-07", "pc2", "acct-07", 2, false, 0, "down"}, {"agent-08", "pc2", "acct-08", 2, false, 22, ""},
		{"agent-09", "kali", "acct-09", 1, true, 17, ""}, {"agent-10", "kali", "acct-10", 1, true, 0, ""},
		{"agent-11", "kali", "acct-11", 1, true, 0, ""}, {"agent-12", "kali", "acct-12", 1, false, 5, ""},
	}
	lent := map[string]bool{"agent-09": true}
	if f.Role == RoleGuest {
		s.Me = Me{Name: "friend@example.com", Role: RoleGuest, Device: "friend-laptop.tail5678.ts.net", Lent: []string{"agent-09"}}
	} else {
		s.Me = Me{Name: "operator", Role: RoleAdmin}
		s.Knocks = len(f.knocks)
	}
	hosts := map[string]*Host{}
	i := 0
	for _, a := range agents {
		if f.Role == RoleGuest && !lent[a.agent] {
			continue
		}
		cont := a.agent
		for k := 1; k <= a.slots; k++ {
			name := a.agent
			if k > 1 {
				name = fmt.Sprintf("%s:%d", a.agent, k)
			}
			w := Worker{Agent: name, SlotOf: a.agent, Slot: k, Host: a.host, Container: cont, Account: a.account,
				Models: []string{"sonnet", "opus"}, Model: "opus", Sandbox: a.sandbox, Alerts: []string{},
				FiveHour: a.five + float64(f.step), SevenDay: a.five / 3, FiveResets: now.Add(time.Duration(90+i*7) * time.Minute),
				QuotaAt: now.Add(-time.Minute), Context: float64((i*13 + f.step) % 70), RecordedAt: now.Add(-3 * time.Second)}
			switch {
			case a.state == "down":
				w.State, w.Reason, w.Alerts = "down", "container is not running", []string{"down"}
				w.Activity = Activity{Kind: ActDown}
			case a.state == "cooling" && k == 1:
				w.State, w.Cooling = "cooling", now.Add(25*time.Minute)
				w.Activity = Activity{Kind: ActCooling}
			case open[name] != "":
				w.Task = open[name]
				if strings.HasPrefix(w.Task, "20261025-access") {
					w.State, w.Activity = "waiting", Activity{Kind: ActWaiting, Since: now.Add(-time.Minute)}
				} else {
					act := fakeActs[(i+f.step)%len(fakeActs)]
					w.State, w.Activity = "working", Activity{Kind: act, Tool: fakeTools[act], Since: now.Add(-time.Duration(5+i) * time.Second)}
				}
			default:
				w.State, w.Idle, w.IdleS = "standby", fmt.Sprintf("%dm", 20+i), int64((20+i)*60)
				w.Activity = Activity{Kind: ActIdle}
			}
			w.Low = w.FiveHour >= 95
			s.Workers = append(s.Workers, w)
			i++
		}
		h := hosts[a.host]
		if h == nil {
			h = &Host{Name: a.host, Reachable: true}
			hosts[a.host] = h
		}
		c := Container{Name: cont, Agent: a.agent, Running: a.state != "down", CPU: float64((i*17+f.step*5)%90) + 3,
			Mem: int64(900+i*60) << 20, MemLimit: 2560 << 20, At: now}
		if a.sandbox {
			c.MemLimit = 1536 << 20
		}
		c.CPUHist, c.MemHist = series(now, 60, 10*time.Second, float64(i), 40, 30), series(now, 60, 10*time.Second, float64(i)+1, 1000, 200)
		h.Containers = append(h.Containers, c)
	}
	for _, name := range []string{"pc2", "kali"} {
		h := hosts[name]
		if h == nil {
			continue
		}
		if name == "pc2" {
			h.CPUs, h.MemTotal, h.MemUsed, h.DiskTotal, h.DiskUsed = 4, 7<<30, 5<<30+int64(f.step)<<20, 233<<30, 158<<30
			h.Load = [3]float64{2.4, 2.9, 3.1}
		} else {
			h.CPUs, h.MemTotal, h.MemUsed, h.DiskTotal, h.DiskUsed = 8, 16<<30, 9<<30, 467<<30, 301<<30
			h.Load = [3]float64{1.2, 1.6, 1.8}
		}
		h.At, h.Load1 = now, series(now, 60, 10*time.Second, float64(len(name)), h.Load[0], 1)
		s.Hosts = append(s.Hosts, *h)
	}
	for _, t := range f.tasks {
		if f.Role == RoleGuest && t.Assigner != s.Me.Name {
			continue
		}
		s.Tasks = append(s.Tasks, t)
	}
	accounts := map[string]bool{}
	for _, w := range s.Workers {
		if accounts[w.Account] {
			continue
		}
		accounts[w.Account] = true
		a := Account{Name: w.Account, Agents: []string{w.SlotOf}, FiveHour: w.FiveHour, SevenDay: w.SevenDay,
			FiveResets: w.FiveResets, At: w.QuotaAt, BurnPerHour: float64(len(s.Quota)%4) * 6.5}
		if a.BurnPerHour > 0 {
			if out := now.Add(time.Duration((100 - a.FiveHour) / a.BurnPerHour * float64(time.Hour))); out.Before(a.FiveResets) {
				a.RunOut = out
			}
		}
		a.History = series(now, 30, 10*time.Minute, float64(len(s.Quota)), math.Max(a.FiveHour-20, 0), 10)
		s.Quota = append(s.Quota, a)
	}
	if f.Role == RoleAdmin {
		s.Quota = append(s.Quota, Account{Name: "karmax", Orchestrator: true, FiveHour: 2, SevenDay: 6, FiveResets: now.Add(3 * time.Hour), At: now})
	}
	kinds := []struct{ agent, kind, detail string }{
		{"agent-01", "fleet.report", `{"task":"20261025-tui-frame-1a2b","text":"frame and tabs render at 80x24"}`},
		{"agent-04", "fleet.question", `{"task":"20261025-access-3c4d","text":"Should a lend to an admin device be refused?"}`},
		{"agent-09", "fleet.report", `{"task":"20261025-port-api-7a8b","text":"synced your repo into /work/code/api"}`},
		{"agent-07", "fleet.alert", `{"alert":"down","text":"agent-07 is down: container is not running"}`},
		{"agent-06", "fleet.task.closed", `{"task":"20261025-fix-login-9c0d","outcome":"done"}`},
		{"agent-05:2", "fleet.term", `{"who":"your-mac","mode":"watch","duration_s":95}`},
		{"", "fleet.access.knock", `{"device":"pal-desktop.tail9abc.ts.net","login":"pal@example.com"}`},
	}
	for j, k := range kinds {
		if f.Role == RoleGuest && k.agent != "agent-09" {
			continue
		}
		s.Events = append(s.Events, Event{ID: int64(1000 - j), At: now.Add(-time.Duration(j*3+1) * time.Minute), Agent: k.agent, Kind: k.kind, Detail: []byte(k.detail)})
	}
	return s
}

// series is n points ending at now, every step: a smooth made-up wave.
func series(now time.Time, n int, step time.Duration, phase, mid, amp float64) []Point {
	out := make([]Point, n)
	for i := range out {
		v := mid + amp*math.Sin(phase+float64(i)/5)
		out[i] = Point{T: now.Add(-time.Duration(n-1-i) * step), V: math.Max(v, 0)}
	}
	return out
}

func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func (f *Fake) Screens(ctx context.Context, workers []string, lines int) ([]Screen, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Screen
	for _, w := range workers {
		body := []string{
			"\x1b[38;5;208m✻\x1b[0m Welcome to Claude Code",
			"",
			"> " + w + ": working on its task",
			"",
			fmt.Sprintf("\x1b[2m⏺ Bash(go test ./internal/... -run Test%d)\x1b[0m", f.step),
			"  ⎿  ok  	github.com/example/fleet/internal/access	0.41s",
			"",
			"\x1b[38;5;141m✶ Pondering… (12s · ↓ 1.2k tokens)\x1b[0m",
		}
		if lines > 0 && len(body) > lines {
			body = body[len(body)-lines:]
		}
		out = append(out, Screen{Worker: w, Text: strings.Join(body, "\n"), Cols: 100, At: f.Now()})
	}
	return out, nil
}

func (f *Fake) Task(ctx context.Context, id string) (*TaskDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tasks {
		if t.ID == id {
			state := "open"
			if !t.Closed.IsZero() {
				state = "closed"
			}
			return &TaskDetail{Task: t, State: state, Reports: []Message{
				{ID: 1, From: t.Assigner, To: t.Agent, Text: "the brief for " + id, At: t.Assigned, Delivered: true, Task: id, Kind: "report"},
				{ID: 2, From: t.Agent, To: t.Assigner, Text: t.Last, At: t.LastAt, Delivered: true, Task: id, Kind: t.LastKind},
			}}, nil
		}
	}
	return nil, fmt.Errorf("no task %q", id)
}

func (f *Fake) record(format string, a ...any) {
	f.Calls = append(f.Calls, fmt.Sprintf(format, a...))
	f.seq++
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *Fake) Run(ctx context.Context, r RunRequest) (*RunReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	agent := r.Agent
	if agent == "" {
		agent = "agent-08"
		if f.Role == RoleGuest {
			agent = "agent-09"
		}
	}
	id := fmt.Sprintf("20261025-task-%d", len(f.tasks)+1)
	assigner := "operator"
	if f.Role == RoleGuest {
		assigner = "friend@example.com"
	}
	f.tasks = append([]Task{{ID: id, Agent: agent, Assigner: assigner, Repo: r.Repo, Model: r.Model, Assigned: f.Now(), AdminWork: f.Role == RoleAdmin}}, f.tasks...)
	f.record("run %s %q", agent, r.Brief)
	return &RunReply{Task: id, Agent: agent, Why: "free, lowest 5h use"}, nil
}

func (f *Fake) closeTask(id, outcome string) error {
	for i := range f.tasks {
		if f.tasks[i].ID == id {
			if !f.tasks[i].Closed.IsZero() {
				return fmt.Errorf("task %s is already closed", id)
			}
			f.tasks[i].Closed, f.tasks[i].Outcome = f.Now(), outcome
			return nil
		}
	}
	return fmt.Errorf("no task %q", id)
}

func (f *Fake) Followup(ctx context.Context, task, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("followup %s %q", task, text)
	return nil
}

func (f *Fake) Answer(ctx context.Context, task, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("answer %s %q", task, text)
	return nil
}

func (f *Fake) Cancel(ctx context.Context, task, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.closeTask(task, "cancelled"); err != nil {
		return err
	}
	f.record("cancel %s %q", task, reason)
	return nil
}

func (f *Fake) Done(ctx context.Context, task string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.closeTask(task, "done"); err != nil {
		return err
	}
	f.record("done %s", task)
	return nil
}

func (f *Fake) SetModel(ctx context.Context, worker, model string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Role != RoleAdmin {
		return fmt.Errorf("403: model is for admins")
	}
	f.record("model %s %s", worker, model)
	return nil
}

func (f *Fake) Access(ctx context.Context) (*AccessList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Role != RoleAdmin {
		return nil, fmt.Errorf("403: access is for admins")
	}
	return &AccessList{Devices: append([]Device(nil), f.devices...), Knocks: append([]Knock(nil), f.knocks...)}, nil
}

func (f *Fake) ChangeAccess(ctx context.Context, c AccessChange) (*AccessList, error) {
	f.mu.Lock()
	if f.Role != RoleAdmin {
		f.mu.Unlock()
		return nil, fmt.Errorf("403: access is for admins")
	}
	f.record("access %s %s %s", c.Op, c.Device, strings.Join(c.Agents, ","))
	f.mu.Unlock()
	return f.Access(ctx)
}
