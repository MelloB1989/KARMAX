// Package daemon is fleetd's core: every tick it observes each agent,
// records what it saw, decides (reconcile.Decide) and acts. It is also the
// fleet's system of record — assignments, completions, relays, archives and
// restores all go through here and into the ledger.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/agent"
	"github.com/MelloB1989/karmax/fleet/internal/archive"
	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// Box is one container, as fleetd uses it. *agent.Agent is the real one.
type Box interface {
	archive.Source
	archive.Sink
	Observe(ctx context.Context, now time.Time) (reconcile.Obs, error)
	AckEvents(ctx context.Context) error
	Restart(ctx context.Context, sessionID string) error
	Fresh(ctx context.Context) error
	Prompt(ctx context.Context, text string) error
	StopPID(ctx context.Context, pid int) error
	DeleteTranscript(ctx context.Context, sid string) error
	RemoveWorktree(ctx context.Context, w agent.Worktree) error
	Relay(ctx context.Context, to, from, text string) error
}

// Pusher tells the orchestrator about a state change.
type Pusher interface {
	Push(ctx context.Context, event, agent, text string, st reconcile.State) error
}

// Notifier tells you, on your phone.
type Notifier interface {
	Notify(ctx context.Context, text string) error
}

// Fleet is fleetd's state.
type Fleet struct {
	cfg    *config.Config
	db     *ledger.DB
	box    func(name string) Box
	push   Pusher
	notify Notifier

	// One lock per agent, held for a whole tick of that agent and for every
	// API call that changes it: an assignment must not land between a tick
	// reading the state and writing it back.
	locks  map[string]*sync.Mutex
	mu     sync.Mutex // guards states
	states map[string]reconcile.State

	lastRetain time.Time
	// HostStats, when set, samples container resource use per host.
	HostStats func(ctx context.Context, host string) ([]ledger.HostSample, error)
	// Log receives fleetd's own diagnostics.
	Log func(format string, args ...any)
}

// New loads the fleet's remembered state.
func New(cfg *config.Config, db *ledger.DB, box func(string) Box, push Pusher, notify Notifier) (*Fleet, error) {
	states, err := db.States()
	if err != nil {
		return nil, err
	}
	f := &Fleet{cfg: cfg, db: db, box: box, push: push, notify: notify,
		locks: map[string]*sync.Mutex{}, states: map[string]reconcile.State{}, Log: func(string, ...any) {}}
	for _, name := range cfg.AgentNames() {
		a := cfg.Agents[name]
		st, ok := states[name]
		if !ok {
			st = reconcile.State{Name: name, Phase: reconcile.Standby}
		}
		f.states[name] = st
		f.locks[name] = &sync.Mutex{}
		_ = db.UpsertAgent(ledger.Agent{Name: name, Host: a.Host, Account: a.Account, Models: a.Models,
			Container: a.Container, Image: cfg.Image, Enabled: st.Phase != reconcile.Disabled, Reason: st.DisabledReason})
	}
	return f, nil
}

func (f *Fleet) archiveRoot() string { return f.cfg.StateDir + "/archive" }

// State is an agent's current state.
func (f *Fleet) State(name string) reconcile.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[name]
}

func (f *Fleet) setState(st reconcile.State) {
	f.mu.Lock()
	f.states[st.Name] = st
	f.mu.Unlock()
	if err := f.db.SaveState(st); err != nil {
		f.Log("ledger: save state of %s: %v", st.Name, err)
	}
}

func (f *Fleet) event(name, kind, session string, detail any) {
	var d string
	if detail != nil {
		b, _ := json.Marshal(detail)
		d = string(b)
	}
	if err := f.db.AddEvent(ledger.Event{Agent: name, At: time.Now(), Kind: kind, Session: session, Detail: d}); err != nil {
		f.Log("ledger: event %s: %v", kind, err)
	}
}

// Tick is one pass over every agent, run concurrently.
func (f *Fleet) Tick(ctx context.Context, now time.Time) {
	var wg sync.WaitGroup
	for _, name := range f.cfg.AgentNames() {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			f.tickOne(ctx, name, now)
		}(name)
	}
	if f.HostStats != nil {
		for _, h := range f.cfg.HostNames() {
			wg.Add(1)
			go func(h string) {
				defer wg.Done()
				samples, err := f.HostStats(ctx, h)
				if err != nil {
					return
				}
				for _, s := range samples {
					_ = f.db.AddHostSample(s)
				}
			}(h)
		}
	}
	wg.Wait()
	if now.Sub(f.lastRetain) > 24*time.Hour {
		th := f.cfg.Thresholds
		if err := f.db.Retain(now, ledger.Retention{EventsRaw: th.EventsRaw, QuotaRaw: th.QuotaRaw,
			Relays: th.RelayText, HostSamples: th.HostSamples}); err != nil {
			f.Log("ledger: retention: %v", err)
		}
		f.lastRetain = now
	}
}

func (f *Fleet) tickOne(ctx context.Context, name string, now time.Time) {
	lock := f.locks[name]
	lock.Lock()
	defer lock.Unlock()
	b := f.box(name)
	o, err := b.Observe(ctx, now)
	if err != nil && o.ContainerUp {
		// Could not see inside a running container. Deciding on a blank
		// observation would read as "no session" and restart a healthy agent.
		f.Log("observe %s: %v", name, err)
		f.event(name, "fleet.observe_failed", "", map[string]string{"error": err.Error()})
		return
	}
	for _, e := range o.Events {
		var detail string
		if len(e.Raw) > 0 {
			detail = string(e.Raw)
		} else if e.Tool != "" {
			b, _ := json.Marshal(map[string]any{"tool": e.Tool, "ok": e.OK})
			detail = string(b)
		}
		_ = f.db.AddEvent(ledger.Event{Agent: name, At: e.Time(), Kind: e.Event, Session: e.Session, Detail: detail})
	}
	if len(o.Events) > 0 {
		if err := b.AckEvents(ctx); err != nil {
			f.Log("ack events %s: %v", name, err)
		}
	}
	if s := o.Status; s != nil {
		five, okF := s.FiveHour()
		seven, okS := s.SevenDay()
		if okF || okS {
			_ = f.db.AddQuota(ledger.Quota{Account: f.cfg.Agents[name].Account, At: now,
				FiveHour: five.Used, FiveResets: five.Resets, SevenDay: seven.Used, SevenResets: seven.Resets, Source: "statusline"})
		}
	}

	old := f.State(name)
	next, acts := reconcile.Decide(f.cfg.Thresholds, old, o)
	if !f.execute(ctx, name, b, old, next, acts, now) {
		// Keep what was observed, forget what was decided: the next tick
		// decides again from the same place.
		old.Quota, old.Low, old.Reserved = next.Quota, next.Low, next.Reserved
		old.ContextPct, old.CostUSD = next.ContextPct, next.CostUSD
		next = old
	}
	f.setState(next)
}

// execute runs one tick's actions. It reports false when an action that
// changes sessions failed, so the decision is retried rather than recorded.
func (f *Fleet) execute(ctx context.Context, name string, b Box, old, next reconcile.State, acts []reconcile.Action, now time.Time) bool {
	for _, a := range acts {
		var err error
		switch a.Kind {
		case reconcile.Restart:
			err = b.Restart(ctx, a.Session)
			f.event(name, "fleet.restart", a.Session, nil)
		case reconcile.Rotate:
			if err = f.rotate(ctx, name, b, old, a, now); err != nil {
				f.event(name, "fleet.archive_failed", a.Session, map[string]string{"error": err.Error(), "reason": a.Reason})
				f.Log("rotate %s: %v", name, err)
				return false
			}
		case reconcile.ArchiveStray:
			m, aerr := f.archiveSession(ctx, name, b, old, a.Session, "", reconcile.ReasonStray, now)
			if aerr != nil {
				f.event(name, "fleet.archive_failed", a.Session, map[string]string{"error": aerr.Error(), "reason": a.Reason})
				continue // a stray is retried next tick; the agent's own decision stands
			}
			if err = b.StopPID(ctx, a.PID); err == nil && m != nil {
				err = b.DeleteTranscript(ctx, a.Session)
			}
			f.event(name, "fleet.stray_archived", a.Session, map[string]any{"pid": a.PID})
		case reconcile.Compact:
			err = b.Prompt(ctx, "/compact")
			f.event(name, "fleet.compact", a.Session, nil)
		case reconcile.Prompt:
			err = b.Prompt(ctx, a.Text)
			f.event(name, "fleet.prompt", "", map[string]string{"text": a.Text})
		case reconcile.Push:
			if f.push != nil {
				err = f.push.Push(ctx, a.Event, name, a.Text, next)
			}
			f.event(name, a.Event, "", map[string]string{"text": a.Text})
		case reconcile.Alert:
			if f.notify != nil {
				err = f.notify.Notify(ctx, a.Text)
			}
			f.event(name, "fleet.alert", "", map[string]string{"text": a.Text})
		}
		if err != nil {
			f.Log("%s %s: %v", a.Kind, name, err)
		}
	}
	if next.Phase == reconcile.Disabled && old.Phase != reconcile.Disabled {
		a := f.cfg.Agents[name]
		_ = f.db.UpsertAgent(ledger.Agent{Name: name, Host: a.Host, Account: a.Account, Models: a.Models,
			Container: a.Container, Enabled: false, Reason: next.DisabledReason})
	}
	return true
}

// rotate ends the agent's session — archived and verified first — and starts
// a fresh one. Worktrees with nothing to lose are removed; the rest are kept.
func (f *Fleet) rotate(ctx context.Context, name string, b Box, st reconcile.State, a reconcile.Action, now time.Time) error {
	m, err := f.archiveSession(ctx, name, b, st, a.Session, a.Task, a.Reason, now)
	if err != nil {
		return err
	}
	if err := b.Fresh(ctx); err != nil {
		return err
	}
	f.event(name, "fleet.rotate", a.Session, map[string]string{"reason": a.Reason, "task": a.Task})
	if a.Task != "" && a.Reason != reconcile.ReasonTaskDone {
		_ = f.db.CloseTask(a.Task, now, a.Reason, "", "")
	}
	if m == nil {
		return nil
	}
	if err := b.DeleteTranscript(ctx, a.Session); err != nil {
		f.Log("delete transcript %s/%s: %v", name, a.Session, err)
	}
	for _, w := range m.Worktrees {
		if !w.Safe() {
			continue
		}
		if err := b.RemoveWorktree(ctx, w); err != nil {
			f.Log("remove worktree %s: %v", w.Path, err)
		}
	}
	return nil
}

// archiveSession archives one session and records it in the ledger. A
// session with no transcript (it never had a turn) returns a nil manifest.
func (f *Fleet) archiveSession(ctx context.Context, name string, b Box, st reconcile.State, sid, task, reason string, now time.Time) (*archive.Manifest, error) {
	if sid == "" {
		return nil, nil
	}
	a := f.cfg.Agents[name]
	m, err := archive.Archive(ctx, b, f.archiveRoot(), archive.Info{Agent: name, Host: a.Host, Account: a.Account,
		SessionID: sid, Name: name, Task: task, Reason: reason, At: now})
	if errors.Is(err, agent.ErrNoTranscript) {
		_ = f.db.EndSession(ledger.Session{ID: sid, Agent: name, Task: task, Ended: now, Reason: reason})
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	wts, _ := json.Marshal(m.Worktrees)
	if err := f.db.EndSession(ledger.Session{ID: sid, Agent: name, Task: task, Started: m.Started, Ended: now,
		Reason: reason, Turns: m.Turns, Input: m.Usage.Input, Output: m.Usage.Output, CacheRead: m.Usage.CacheRead,
		CacheCreation: m.Usage.CacheCreation, CostUSD: m.CostUSD, PeakContext: st.ContextPct, Cwd: m.Cwd,
		Worktrees: string(wts), ArchiveID: m.ID}); err != nil {
		f.Log("ledger: end session %s: %v", sid, err)
	}
	f.event(name, "fleet.archive", sid, map[string]any{"id": m.ID, "kept_worktrees": len(m.KeptWorktrees())})
	return m, nil
}

func (f *Fleet) known(name string) error {
	if _, ok := f.cfg.Agents[name]; !ok {
		return fmt.Errorf("no agent named %q", name)
	}
	return nil
}

// Assign records that an agent was given a task. The orchestrator calls this
// (fleetctl assign) when it messages an agent with work.
func (f *Fleet) Assign(name, task string, now time.Time) error {
	if err := f.known(name); err != nil {
		return err
	}
	f.locks[name].Lock()
	defer f.locks[name].Unlock()
	st := f.State(name)
	if st.Phase == reconcile.Disabled {
		return fmt.Errorf("%s is disabled (%s)", name, st.DisabledReason)
	}
	st.Task, st.Done, st.NudgedAt, st.LastBusy = task, false, time.Time{}, now
	f.setState(st)
	f.event(name, "fleet.assign", st.SessionID, map[string]string{"task": task})
	return f.db.OpenTask(task, name, now)
}

// Done records a task's end; the agent's session is archived and replaced
// with a fresh one on the next idle tick. One task, one session.
func (f *Fleet) Done(name, task, outcome, branch, pr string, now time.Time) error {
	if err := f.known(name); err != nil {
		return err
	}
	f.locks[name].Lock()
	defer f.locks[name].Unlock()
	st := f.State(name)
	if task == "" {
		task = st.Task
	}
	if st.Task == "" {
		st.Task = task
	}
	st.Done = true
	f.setState(st)
	f.event(name, "fleet.done", st.SessionID, map[string]string{"task": task, "outcome": outcome})
	if task == "" {
		return nil
	}
	if outcome == "" {
		outcome = "done"
	}
	return f.db.CloseTask(task, now, outcome, branch, pr)
}

// Rotate archives the agent's session and starts a fresh one on its next
// idle tick, as if its task were done.
func (f *Fleet) Rotate(name string, now time.Time) error {
	return f.Done(name, "", "rotated", "", "", now)
}

// Enable clears a disabled agent, once you have fixed its subscription.
func (f *Fleet) Enable(name string) error {
	if err := f.known(name); err != nil {
		return err
	}
	f.locks[name].Lock()
	defer f.locks[name].Unlock()
	st := f.State(name)
	if st.Phase == reconcile.Disabled {
		st.Phase, st.DisabledReason = reconcile.Standby, ""
		delete(st.Alerted, "disabled")
	}
	f.setState(st)
	a := f.cfg.Agents[name]
	_ = f.db.UpsertAgent(ledger.Agent{Name: name, Host: a.Host, Account: a.Account, Models: a.Models, Container: a.Container, Enabled: true})
	f.event(name, "fleet.enable", "", nil)
	return nil
}

// Tell relays a message across hosts: a throwaway session in the receiver's
// container delivers it with SendMessage, so it arrives as a peer message.
func (f *Fleet) Tell(ctx context.Context, from, to, text string, now time.Time) error {
	if to != f.cfg.Orchestrator.Name {
		if err := f.known(to); err != nil {
			return err
		}
	}
	err := f.box(to).Relay(ctx, to, from, text)
	r := ledger.Relay{From: from, To: to, Text: text, At: now, Delivered: err == nil}
	if err != nil {
		r.Error = err.Error()
	}
	_ = f.db.AddRelay(r)
	f.event(to, "fleet.relay", "", map[string]any{"from": from, "delivered": err == nil, "size": len(text)})
	return err
}

// Restore puts an archived session back on an agent (its own by default):
// the current session is archived first, then the restored one resumed.
func (f *Fleet) Restore(ctx context.Context, id, onto string, now time.Time) error {
	m, err := archive.Load(f.archiveRoot(), id)
	if err != nil {
		return err
	}
	name := onto
	if name == "" {
		name = m.Agent
	}
	if err := f.known(name); err != nil {
		return err
	}
	f.locks[name].Lock()
	defer f.locks[name].Unlock()
	b := f.box(name)
	st := f.State(name)
	cur := st.SessionID
	var curM *archive.Manifest
	if cur != "" && cur != m.SessionID {
		if curM, err = f.archiveSession(ctx, name, b, st, cur, st.Task, reconcile.ReasonRestored, now); err != nil {
			return fmt.Errorf("archiving the current session first: %w", err)
		}
	}
	if _, err := archive.Restore(ctx, b, f.archiveRoot(), id); err != nil {
		return err
	}
	if err := b.Restart(ctx, m.SessionID); err != nil {
		return err
	}
	if curM != nil {
		_ = b.DeleteTranscript(ctx, cur)
	}
	st.SessionID, st.Task, st.Done, st.Phase = m.SessionID, "", false, reconcile.Standby
	st.LastBusy, st.NudgedAt, st.CompactedAt = now, time.Time{}, 0
	f.setState(st)
	f.event(name, "fleet.restore", m.SessionID, map[string]string{"archive": id, "replaced": cur})
	return nil
}

// Row is one agent in the roster.
type Row struct {
	Agent       string    `json:"agent"`
	Host        string    `json:"host"`
	Account     string    `json:"account"`
	State       string    `json:"state"`
	Task        string    `json:"task,omitempty"`
	Idle        string    `json:"idle"`
	FiveHour    float64   `json:"five_hour_pct"`
	SevenDay    float64   `json:"seven_day_pct"`
	FiveResets  time.Time `json:"five_hour_resets,omitzero"`
	SevenResets time.Time `json:"seven_day_resets,omitzero"`
	QuotaAt     time.Time `json:"quota_as_of,omitzero"`
	Context     float64   `json:"context_pct"`
	Low         bool      `json:"low,omitempty"`
	Reserved    bool      `json:"reserved,omitempty"`
	Models      []string  `json:"models"`
	Session     string    `json:"session,omitempty"`
	Reason      string    `json:"disabled_reason,omitempty"`
	Cooling     time.Time `json:"cooling_until,omitzero"`
}

// Status is the roster, one row per agent, sorted.
func (f *Fleet) Status(now time.Time) []Row {
	var rows []Row
	for _, name := range f.cfg.AgentNames() {
		st, a := f.State(name), f.cfg.Agents[name]
		idle := ""
		if !st.LastBusy.IsZero() && st.Phase != reconcile.Working {
			idle = Dur(now.Sub(st.LastBusy))
		}
		rows = append(rows, Row{Agent: name, Host: a.Host, Account: a.Account, State: string(st.Phase), Task: st.Task,
			Idle: idle, FiveHour: st.Quota.FiveHour, SevenDay: st.Quota.SevenDay, FiveResets: st.Quota.FiveHourResets,
			SevenResets: st.Quota.SevenDayResets, QuotaAt: st.Quota.At, Context: st.ContextPct, Low: st.Low,
			Reserved: st.Reserved, Models: a.Models, Session: st.SessionID, Reason: st.DisabledReason, Cooling: st.CoolingUntil})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Agent < rows[j].Agent })
	return rows
}

// Dur is a short human duration: 45s, 10m, 3h20m, 2d4h.
func Dur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) % 60; m != 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	default:
		days := int(d.Hours()) / 24
		return fmt.Sprintf("%dd%dh", days, int(d.Hours())%24)
	}
}

// DB exposes the ledger for read-only queries.
func (f *Fleet) DB() *ledger.DB { return f.db }

// Config is the fleet's configuration.
func (f *Fleet) Config() *config.Config { return f.cfg }

// ArchiveRoot is where archives live.
func (f *Fleet) ArchiveRoot() string { return f.archiveRoot() }
