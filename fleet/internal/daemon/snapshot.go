package daemon

import (
	"encoding/json"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// The watch stream: one snapshot of the whole fleet, made when something
// changed (a tick ended, an event landed) and at most once a second, and
// cut to what each caller may see. GET /v1/watch long-polls on Changed.

// snapshotEvents is how many events a snapshot holds; snapshotPool is how
// many of the newest are read to find them (a guest's are a few of many).
const (
	snapshotEvents = 200
	snapshotPool   = 1000
	bumpEvery      = time.Second
)

// stream is the snapshot's sequence and what moves it.
type stream struct {
	mu      sync.Mutex
	seq     uint64
	changed chan struct{}
	bumped  time.Time   // when seq last moved
	due     *time.Timer // a bump held back to keep to one a second
	rethink time.Time   // when a working worker's last tool turns to thinking
	again   *time.Timer
	tools   map[string]ToolEvent // by session, or by agent for an event without one

	tickAt   time.Time
	tickMS   int64
	tickErrs []string // the last tick's
	errs     []string // the tick under way's

	buildMu sync.Mutex
	full    *fullSnapshot
}

// fullSnapshot is the whole fleet at one seq, before it is cut to a caller.
type fullSnapshot struct {
	s     *fleetapi.Snapshot // Events: up to snapshotPool
	usage map[string]ledger.UsageRow
}

func newStream() *stream {
	return &stream{seq: 1, changed: make(chan struct{}), tools: map[string]ToolEvent{}}
}

// SnapshotSeq is the newest snapshot's sequence number. It starts at 1, so a
// watch since 0 answers at once.
func (f *Fleet) SnapshotSeq() uint64 {
	f.watch.mu.Lock()
	defer f.watch.mu.Unlock()
	return f.watch.seq
}

// Changed is closed when the next snapshot is made. Take it before reading
// SnapshotSeq, so a change in between is not missed.
func (f *Fleet) Changed() <-chan struct{} {
	f.watch.mu.Lock()
	defer f.watch.mu.Unlock()
	return f.watch.changed
}

// KnockCount is how many untagged devices asked for access. Device access
// gives it its count; until then there are none.
func (f *Fleet) KnockCount() int { return 0 }

// touch says the fleet changed: a new snapshot now, or within the second
// when one was just made.
func (f *Fleet) touch() {
	w := f.watch
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.due != nil {
		return // one is coming, and sees this change too
	}
	wait := bumpEvery - time.Since(w.bumped)
	if wait <= 0 {
		w.bumpLocked()
		return
	}
	w.due = time.AfterFunc(wait, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.due = nil
		w.bumpLocked()
	})
}

func (w *stream) bumpLocked() {
	w.seq++
	w.bumped = time.Now()
	close(w.changed)
	w.changed = make(chan struct{})
}

// landed is an event on its way into the ledger: the watch stream moves,
// and a tool event becomes its worker's activity. A worker shows a tool for
// thinkAfter, so the stream moves again then, for it to turn to thinking.
func (f *Fleet) landed(e ledger.Event) {
	if e.Kind == "otel.tool_result" || e.Kind == "otel.tool_decision" {
		var d struct {
			Tool    string `json:"tool_name"`
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(e.Detail), &d) == nil && d.Tool != "" {
			key := e.Session
			if key == "" {
				key = e.Agent
			}
			w := f.watch
			w.mu.Lock()
			if old, ok := w.tools[key]; !ok || !e.At.Before(old.At) {
				w.tools[key] = ToolEvent{Tool: d.Tool, Command: d.Command, At: e.At}
			}
			if at := e.At.Add(thinkAfter + time.Second); at.After(w.rethink) {
				w.rethink = at
				if w.again == nil {
					w.again = time.AfterFunc(time.Until(at), f.rethink)
				}
			}
			w.mu.Unlock()
		}
	}
	f.touch()
}

// rethink moves the stream once the newest tool event has gone stale.
func (f *Fleet) rethink() {
	w := f.watch
	w.mu.Lock()
	if d := time.Until(w.rethink); d > 0 {
		w.again = time.AfterFunc(d, f.rethink)
		w.mu.Unlock()
		return
	}
	w.again = nil
	w.mu.Unlock()
	f.touch()
}

// lastTool is a worker's latest tool event: its session's, or, for its first
// slot, one its agent sent without a session.
func (f *Fleet) lastTool(r Row) ToolEvent {
	w := f.watch
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.tools[r.Session]; ok && r.Session != "" {
		return t
	}
	if r.Slot <= 1 {
		return w.tools[r.SlotOf]
	}
	return ToolEvent{}
}

// forgetTools drops tool events too old to show, so the map keeps to the
// sessions at work.
func (f *Fleet) forgetTools(now time.Time) {
	w := f.watch
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, t := range w.tools {
		if now.Sub(t.At) > time.Hour {
			delete(w.tools, k)
		}
	}
}

// tickStarted and tickEnded measure a tick for the snapshot's Fleetd, and
// the end moves the stream. tickErr notes what went wrong in it.
func (f *Fleet) tickStarted() {
	f.watch.mu.Lock()
	f.watch.errs = nil
	f.watch.mu.Unlock()
}

func (f *Fleet) tickEnded(at time.Time, took time.Duration) {
	w := f.watch
	w.mu.Lock()
	w.tickAt, w.tickMS, w.tickErrs, w.errs = at, took.Milliseconds(), w.errs, nil
	w.mu.Unlock()
	f.touch()
}

func (f *Fleet) tickErr(text string) {
	w := f.watch
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.errs) < 20 && !slices.Contains(w.errs, text) { // a host's agents all say it
		w.errs = append(w.errs, text)
	}
}

// Recorder is the ledger as fleetd's telemetry receiver (otlp.Handler)
// writes to it: each event also moves the watch stream, and a tool event
// becomes its worker's activity.
type Recorder struct{ f *Fleet }

func (f *Fleet) Recorder() Recorder { return Recorder{f} }

func (r Recorder) AddUsage(u ledger.Usage) error { return r.f.db.AddUsage(u) }

func (r Recorder) AddEvent(e ledger.Event) error {
	err := r.f.db.AddEvent(e)
	if err == nil {
		r.f.landed(e)
	}
	return err
}

// Snapshot is the fleet as one caller sees it: everything when all, else
// only the agents named (a guest's lent ones), the tasks on them that me
// asked for, and their events, quota and containers. The whole is made once
// per sequence number and shared; each caller gets its own cut.
func (f *Fleet) Snapshot(now time.Time, all bool, agents []string, me fleetapi.Me) *fleetapi.Snapshot {
	seq := f.SnapshotSeq()
	w := f.watch
	w.buildMu.Lock()
	if w.full == nil || w.full.s.Seq != seq {
		w.full = f.buildSnapshot(now, seq)
	}
	full := w.full
	w.buildMu.Unlock()
	if !all {
		return scopeSnapshot(full, agents, me)
	}
	s := *full.s
	s.Me = me
	s.Events = s.Events[:min(len(s.Events), snapshotEvents)]
	return &s
}

// buildSnapshot reads the whole fleet. What cannot be read is left out and
// logged: a snapshot is never refused.
func (f *Fleet) buildSnapshot(now time.Time, seq uint64) *fullSnapshot {
	s := &fleetapi.Snapshot{Seq: seq, At: now, Workers: []fleetapi.Worker{}, Tasks: []fleetapi.Task{},
		Events: []fleetapi.Event{}, Quota: []fleetapi.Account{}, Hosts: []fleetapi.Host{}}
	full := &fullSnapshot{s: s, usage: map[string]ledger.UsageRow{}}
	for _, r := range f.Status(now) {
		cw := f.worker(r.Agent)
		s.Workers = append(s.Workers, fleetapi.Worker{Agent: r.Agent, SlotOf: r.SlotOf, Slot: r.Slot, Host: r.Host,
			Container: cw.Container, Account: r.Account, State: r.State, Task: r.Task, Idle: r.Idle, IdleS: r.IdleS,
			Model: r.Model, Alerts: r.Alerts, RecordedAt: r.RecordedAt, FiveHour: r.FiveHour, SevenDay: r.SevenDay,
			FiveResets: r.FiveResets, SevenResets: r.SevenResets, QuotaAt: r.QuotaAt, Context: r.Context, Low: r.Low,
			Reserved: r.Reserved, Models: r.Models, Session: r.Session, Reason: r.Reason, Cooling: r.Cooling,
			Unreachable: r.Unreachable, Rotating: r.Rotating, Sandbox: cw.Sandbox, Activity: Activity(r, f.lastTool(r), now)})
	}
	if lines, err := f.db.TaskLines(now.Add(-24 * time.Hour)); err != nil {
		f.Log("snapshot: tasks: %v", err)
	} else {
		for _, l := range lines {
			s.Tasks = append(s.Tasks, fleetapi.Task{ID: l.ID, Agent: l.Agent, Assigner: l.Assigner, Outcome: l.Outcome,
				Repo: l.Repo, Model: l.Model, Branch: l.Branch, PR: l.PR, Assigned: l.Assigned, Closed: l.Closed,
				Messages: l.Messages, Last: firstLine(l.Last, 200), LastAt: l.LastAt, LastKind: l.LastKind})
		}
	}
	if evs, err := f.db.EventsWhere(ledger.EventQuery{Newest: true, Limit: snapshotPool}); err != nil {
		f.Log("snapshot: events: %v", err)
	} else {
		for i := len(evs) - 1; i >= 0; i-- {
			e := evs[i]
			if e.Kind == "otel.tool_decision" {
				continue // the activity's signal; its tool_result is the event
			}
			s.Events = append(s.Events, fleetapi.Event{ID: e.ID, At: e.At, Agent: e.Agent, Kind: e.Kind,
				Session: e.Session, Detail: rawDetail(e.Detail)})
		}
	}
	s.Quota = f.accounts(now)
	if t, err := f.Today(now); err != nil {
		f.Log("snapshot: today: %v", err)
	} else {
		s.Today = fleetapi.Today(t)
		if rows, err := f.db.UsageBy("agent", t.Since); err == nil {
			for _, u := range rows {
				full.usage[u.Key] = u
			}
		}
	}
	s.Hosts = f.hostsView(now)
	w := f.watch
	w.mu.Lock()
	s.Fleetd = fleetapi.Fleetd{Version: f.Version, Started: f.Started, TickAt: w.tickAt, TickMS: w.tickMS,
		TickErrors: append([]string(nil), w.tickErrs...)}
	w.mu.Unlock()
	return full
}

// scopeSnapshot cuts the whole fleet to a guest's: its lent agents' workers,
// its own tasks on them, the events of those agents that are not about
// someone else's task, their accounts' quota, and their containers alone of
// the hosts. A lent agent at work on another guest's task shows no task.
func scopeSnapshot(full *fullSnapshot, agents []string, me fleetapi.Me) *fleetapi.Snapshot {
	src := full.s
	lent := map[string]bool{}
	for _, a := range agents {
		lent[agentOf(a)] = true
	}
	s := &fleetapi.Snapshot{Seq: src.Seq, At: src.At, Me: me, Workers: []fleetapi.Worker{}, Tasks: []fleetapi.Task{},
		Events: []fleetapi.Event{}, Quota: []fleetapi.Account{}, Hosts: []fleetapi.Host{},
		Fleetd: fleetapi.Fleetd{Version: src.Fleetd.Version, Started: src.Fleetd.Started}}
	own := map[string]bool{}
	for _, t := range src.Tasks {
		if lent[agentOf(t.Agent)] && ledger.OwnedBy(t.Assigner, me.Name) {
			own[t.ID] = true
			s.Tasks = append(s.Tasks, t)
		}
	}
	accounts := map[string]bool{}
	for _, w := range src.Workers {
		if !lent[agentOf(w.Agent)] {
			continue
		}
		if w.Task != "" && !own[w.Task] {
			w.Task = ""
		}
		accounts[w.Account] = true
		s.Workers = append(s.Workers, w)
	}
	for _, e := range src.Events {
		if len(s.Events) == snapshotEvents {
			break
		}
		if lent[agentOf(e.Agent)] && (eventTask(e) == "" || own[eventTask(e)]) {
			s.Events = append(s.Events, e)
		}
	}
	for _, a := range src.Quota {
		if a.Orchestrator || !accounts[a.Name] {
			continue
		}
		var keep []string
		for _, n := range a.Agents {
			if lent[n] {
				keep = append(keep, n)
			}
		}
		a.Agents = keep
		s.Quota = append(s.Quota, a)
	}
	for _, h := range src.Hosts {
		var cs []fleetapi.Container
		for _, c := range h.Containers {
			if lent[c.Agent] {
				cs = append(cs, c)
			}
		}
		if len(cs) > 0 {
			s.Hosts = append(s.Hosts, fleetapi.Host{Name: h.Name, Reachable: h.Reachable, Containers: cs,
				Load1: []fleetapi.Point{}})
		}
	}
	s.Today.Since = src.Today.Since
	for _, t := range s.Tasks {
		if !t.Closed.IsZero() && !t.Closed.Before(src.Today.Since) {
			s.Today.TasksClosed++
		}
	}
	for a := range lent {
		u := full.usage[a]
		s.Today.SpendUSD += u.CostUSD
		s.Today.OutputTokens += u.Output
	}
	return s
}

// agentOf is the agent a worker (agent-03:2) is a slot of.
func agentOf(worker string) string {
	a, _ := config.SplitWorker(worker)
	return a
}

// eventTask is the task an event is about, when its detail names one.
func eventTask(e fleetapi.Event) string {
	var d struct {
		Task string `json:"task"`
	}
	if len(e.Detail) > 0 && e.Detail[0] == '{' && json.Unmarshal(e.Detail, &d) == nil {
		return d.Task
	}
	return ""
}

// rawDetail is an event's detail as JSON: as it is when it is JSON, else as
// a JSON string.
func rawDetail(d string) json.RawMessage {
	switch {
	case d == "":
		return nil
	case json.Valid([]byte(d)):
		return json.RawMessage(d)
	}
	b, _ := json.Marshal(d)
	return b
}

// firstLine is a message's first non-blank line, cut to n bytes on a rune.
func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// historyStep is the spacing of an account's 5-hour history.
const historyStep = 5 * time.Minute

// accounts is each subscription's quota: its latest windows, the last 5
// hours of its 5-hour window, the burn rate over the last hour and when it
// runs out at that rate. The orchestrator's is last.
func (f *Fleet) accounts(now time.Time) []fleetapi.Account {
	byAccount := map[string][]string{}
	for _, name := range f.cfg.AgentNames() {
		a := f.cfg.Agents[name]
		byAccount[a.Account] = append(byAccount[a.Account], name)
	}
	names := make([]string, 0, len(byAccount))
	for n := range byAccount {
		names = append(names, n)
	}
	sort.Strings(names)
	orch := f.cfg.Orchestrator.Name
	if orch != "" {
		if _, ok := byAccount[orch]; !ok {
			names = append(names, orch)
		}
	}
	latest, err := f.db.LatestQuota()
	if err != nil {
		f.Log("snapshot: quota: %v", err)
	}
	samples, err := f.db.QuotaSince(now.Add(-5 * time.Hour))
	if err != nil {
		f.Log("snapshot: quota history: %v", err)
	}
	hist := map[string][]ledger.Quota{}
	for _, q := range samples {
		hist[q.Account] = append(hist[q.Account], q)
	}
	out := []fleetapi.Account{}
	for _, n := range names {
		q := latest[n]
		a := fleetapi.Account{Name: n, Agents: byAccount[n], Orchestrator: n == orch, FiveHour: q.FiveHour,
			SevenDay: q.SevenDay, FiveResets: q.FiveResets, SevenResets: q.SevenResets, At: q.At, History: []fleetapi.Point{}}
		if a.Agents == nil {
			a.Agents = []string{}
		}
		h := hist[n]
		for _, s := range h {
			p := fleetapi.Point{T: s.At.Truncate(historyStep), V: s.FiveHour}
			if k := len(a.History); k > 0 && a.History[k-1].T.Equal(p.T) {
				a.History[k-1] = p // the bucket's newest
			} else {
				a.History = append(a.History, p)
			}
		}
		a.BurnPerHour = burn(h, q, now)
		a.RunOut = runOut(q, a.BurnPerHour, now)
		out = append(out, a)
	}
	return out
}

// burn is the 5-hour window's growth in points per hour: a least-squares fit
// of the last hour's samples of the window now open (the one latest is in).
func burn(samples []ledger.Quota, latest ledger.Quota, now time.Time) float64 {
	var xs, ys []float64
	for _, s := range samples {
		if s.At.Before(now.Add(-time.Hour)) || s.At.After(now) || !sameWindow(s.FiveResets, latest.FiveResets) {
			continue
		}
		xs = append(xs, s.At.Sub(now).Hours())
		ys = append(ys, s.FiveHour)
	}
	if len(xs) < 2 {
		return 0
	}
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx /= float64(len(xs))
	my /= float64(len(ys))
	var sxy, sxx float64
	for i := range xs {
		sxy += (xs[i] - mx) * (ys[i] - my)
		sxx += (xs[i] - mx) * (xs[i] - mx)
	}
	if sxx == 0 {
		return 0
	}
	return sxy / sxx
}

// sameWindow reports whether two readings of when the 5-hour window resets
// are the same window: they agree within a few minutes.
func sameWindow(a, b time.Time) bool {
	return math.Abs(a.Sub(b).Minutes()) <= 5
}

// runOut is when the 5-hour window reaches 100 % at the burn rate; zero when
// it resets first, or does not grow.
func runOut(q ledger.Quota, rate float64, now time.Time) time.Time {
	if rate <= 0 || q.At.IsZero() {
		return time.Time{}
	}
	at := now.Add(time.Duration((100 - q.FiveHour) / rate * float64(time.Hour)))
	if !q.FiveResets.IsZero() && !at.Before(q.FiveResets) {
		return time.Time{}
	}
	return at
}
