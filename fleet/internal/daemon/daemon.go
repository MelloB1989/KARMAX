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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/agent"
	"github.com/MelloB1989/karmax/fleet/internal/archive"
	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
	"github.com/MelloB1989/karmax/fleet/internal/vault"
)

// Box is one container, as fleetd uses it. *agent.Agent is the real one.
type Box interface {
	archive.Source
	archive.Sink
	Observe(ctx context.Context, now time.Time) (reconcile.Obs, error)
	AckEvents(ctx context.Context) error
	// Start starts the container when Docker left it stopped.
	Start(ctx context.Context) error
	Restart(ctx context.Context, sessionID string) error
	Fresh(ctx context.Context) error
	Prompt(ctx context.Context, text string) error
	Peek(ctx context.Context, n int) (string, error)
	StopPID(ctx context.Context, pid int) error
	DeleteTranscript(ctx context.Context, sid string) error
	RemoveWorktree(ctx context.Context, w agent.Worktree) error
	Relay(ctx context.Context, to, from, text string) error
	// Interrupt presses Escape in the agent's pane, stopping what it is doing.
	Interrupt(ctx context.Context) error
	// AckOutbox removes queued messages from the container's outbox.
	AckOutbox(ctx context.Context, files []string) error
	// KillSessionProcs ends what a session left running, once it is gone.
	KillSessionProcs(ctx context.Context, sid string) (int, error)
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
	runMu  sync.Mutex // Run: pick and assign are one critical section
	states map[string]reconcile.State

	lastRetain time.Time
	// alerted is when each alert text last went out (firstAlert).
	alerted map[string]time.Time
	// relayBackoff is the pauses before a relay that did not get through is
	// tried again (relay).
	relayBackoff []time.Duration
	// holdAfter is how long a message that must not be lost (send) is tried
	// before it is held for later.
	holdAfter time.Duration
	// sendMus keeps each receiver's messages in order: one send or resend to
	// a receiver at a time.
	sendMus map[string]*sync.Mutex
	// resendFailed is when a receiver's pending messages last failed to go
	// out (resend waits resendEvery); resendTries counts each message's tries.
	resendFailed map[string]time.Time
	resendTries  map[int64]int
	// soonEvery is how often an agent whose task is done is looked at until
	// it has rotated (0: only at the tick).
	soonEvery time.Duration
	// waitEvery is how often Wait looks again.
	waitEvery time.Duration
	// asks are the questions agents are blocked on (fleetctl ask), by task.
	askMu sync.Mutex
	asks  map[string]*ask
	// git runs git in a directory; tests replace it.
	git func(ctx context.Context, dir string, args ...string) (string, error)
	// Memory, when set, is the operator's long-term memory, read for callers
	// (Recall); nil is memory off.
	Memory Recaller
	// HostStats, when set, samples container resource use per host.
	HostStats func(ctx context.Context, host string) ([]ledger.HostSample, error)
	// Karmax, when set, is the KARMAX gateway's door to KARMAX's tools.
	Karmax Gateway
	// Log receives fleetd's own diagnostics.
	Log func(format string, args ...any)
	// vault seals the secrets the operator hands agents (secrets.go).
	vault *vault.Vault
}

// New loads the fleet's remembered state.
func New(cfg *config.Config, db *ledger.DB, box func(string) Box, push Pusher, notify Notifier) (*Fleet, error) {
	states, err := db.States()
	if err != nil {
		return nil, err
	}
	f := &Fleet{cfg: cfg, db: db, box: box, push: push, notify: notify,
		locks: map[string]*sync.Mutex{}, states: map[string]reconcile.State{}, Log: func(string, ...any) {},
		waitEvery: 500 * time.Millisecond, git: runGit, relayBackoff: []time.Duration{5 * time.Second, 15 * time.Second},
		soonEvery: 5 * time.Second, holdAfter: 75 * time.Second, sendMus: map[string]*sync.Mutex{},
		resendFailed: map[string]time.Time{}, resendTries: map[int64]int{}, asks: map[string]*ask{},
		vault: vault.New(filepath.Join(cfg.StateDir, vault.KeyFile))}
	for _, name := range append(cfg.AgentNames(), cfg.Orchestrator.Name, "") {
		f.sendMus[name] = &sync.Mutex{}
	}
	// What the last fleetd was sending when it stopped is sent again, once
	// its receiver is up (resend).
	if _, err := db.InterruptRelays(); err != nil {
		return nil, err
	}
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

// alertEvery is how often one alert text may go out: every agent on a host
// that is out of reach says the same thing in the same tick.
const alertEvery = 15 * time.Minute

// firstAlert reports whether an alert's text has not gone out in the last
// alertEvery, and records that it goes out now.
func (f *Fleet) firstAlert(text string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alerted == nil {
		f.alerted = map[string]time.Time{}
	}
	if t, ok := f.alerted[text]; ok && now.Sub(t) < alertEvery {
		return false
	}
	for k, t := range f.alerted {
		if now.Sub(t) >= alertEvery {
			delete(f.alerted, k)
		}
	}
	f.alerted[text] = now
	return true
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
	if o := f.cfg.Orchestrator.Name; o != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.resend(ctx, o, f.box(o), now) // agents' reports cut short on their way
		}()
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
	f.escalate(ctx, now)
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
	if err != nil {
		// Its host did not answer whether the container runs (Observe asks
		// that first): the agent is out of reach, not down.
		host := f.cfg.Agents[name].Host
		if f.State(name).UnreachableSince.IsZero() {
			f.Log("observe %s: %s does not answer: %v", name, host, err)
			f.event(name, "fleet.unreachable", "", map[string]string{"host": host, "error": err.Error()})
		}
		// The host alone: the error names this agent's container, and the
		// host's agents must raise one alert, not one each (firstAlert).
		o = reconcile.Obs{Now: now, Unreachable: host}
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
	if old.Task != "" {
		if o.Owes, err = f.db.Owes(old.Task); err != nil {
			f.Log("ledger: owes %s: %v", old.Task, err)
		}
		if o.Asking, o.AnsweredAt, err = f.db.Asking(old.Task); err != nil {
			f.Log("ledger: asking %s: %v", old.Task, err)
		}
		// An ask blocked on the answer holds its agent's turn open: asking, even
		// before the ledger has the question.
		f.askMu.Lock()
		_, blocked := f.asks[old.Task]
		f.askMu.Unlock()
		o.Asking = o.Asking || blocked
	}
	next, acts := reconcile.Decide(f.cfg.Thresholds, old, o)
	if !old.UnreachableSince.IsZero() && next.UnreachableSince.IsZero() {
		f.event(name, "fleet.reachable", "", map[string]string{"host": f.cfg.Agents[name].Host, "after": Dur(now.Sub(old.UnreachableSince))})
	}
	if !f.execute(ctx, name, b, old, next, acts, now) {
		// Keep what was observed, forget what was decided: the next tick
		// decides again from the same place.
		old.Quota, old.Low, old.Reserved = next.Quota, next.Low, next.Reserved
		old.ContextPct, old.CostUSD = next.ContextPct, next.CostUSD
		next = old
	}
	next.RecordedAt = now
	if s := o.Status; s != nil {
		if m := firstNonEmpty(s.Model.DisplayName, s.Model.ID); m != "" {
			next.Model, next.ModelID = m, s.Model.ID
		}
		if o.ContainerUp && (s.SessionID == "" || s.SessionID == next.SessionID) {
			idle := (next.Phase == reconcile.Standby || next.Phase == reconcile.Waiting) && next.WaitingSince.IsZero()
			next = f.applyModel(ctx, name, b, next, next.Model != "", idle, now)
		}
	}
	f.setState(next)
	switch next.Phase {
	case reconcile.Standby, reconcile.Waiting, reconcile.Working:
		if o.ContainerUp && next.SessionID != "" { // seen up this tick, with its session
			f.resend(ctx, name, b, now)
		}
	}
	if len(o.Outbox) > 0 {
		f.deliverOutbox(ctx, name, b, o.Outbox, now)
	}
}

// outboxGiveUp is how long a queued message is tried before it is dropped
// and you are told.
const outboxGiveUp = time.Hour

// deliverOutbox sends what the agent queued while fleetd did not answer, as
// the agent's — whatever the file says it is from — and removes each once it
// is sent. One that cannot go now is tried next tick, for outboxGiveUp; one
// to nobody fleetd knows is dropped.
func (f *Fleet) deliverOutbox(ctx context.Context, name string, b Box, msgs []reconcile.Outboxed, now time.Time) {
	var gone []string
	for _, m := range msgs {
		at := m.At
		if at.IsZero() || at.After(now) {
			at = now
		}
		err := f.deliverMsg(ctx, name, m.To, m.Text, "", m.Kind, at, true)
		var held *Held
		f.event(name, "fleet.outbox", "", map[string]any{"to": m.To, "queued_at": at, "delivered": err == nil, "held": errors.As(err, &held)})
		switch {
		case err == nil, held != nil: // held: on the ledger now, and fleetd's to send
			gone = append(gone, m.File)
		case m.To != f.cfg.Orchestrator.Name && !f.inbox(m.To) && f.known(m.To) != nil, now.Sub(at) > outboxGiveUp:
			gone = append(gone, m.File)
			text := fmt.Sprintf("%s's message to %s, queued while fleetd was away, could not be delivered: %v", name, m.To, err)
			f.Log("%s", text)
			if f.notify != nil {
				_ = f.notify.Notify(ctx, text)
			}
			f.event(name, "fleet.alert", "", map[string]string{"text": text})
		default:
			f.Log("outbox %s → %s: %v; trying again next tick", name, m.To, err)
		}
	}
	if len(gone) > 0 {
		if err := b.AckOutbox(ctx, gone); err != nil {
			f.Log("ack outbox %s: %v", name, err)
		}
	}
}

// resendWindow is how old a message cut short may be and still be resent.
const resendWindow = 24 * time.Hour

// resendNote heads a message sent again because fleetd stopped while it was
// on its way: it may have arrived after all. heldNote heads one fleetd kept
// while the receiver could not take it.
const (
	resendNote = "[fleetd restarted while it was delivering this message, so it is sent again. If you already have it, ignore this copy.]\n"
	heldNote   = "[fleetd could not reach you when this was sent (%s), so it comes now. If you already have it, ignore this copy.]\n"
)

// resendEvery is how long a receiver whose pending messages did not go out
// is left before they are tried again; resendTries is how many tries a
// message gets before it is dropped and you are told.
const (
	resendEvery = 30 * time.Second
	resendTries = 5
)

// resend sends the receiver what is pending for it — what a previous fleetd
// stopped in the middle of sending, and what was held while it could not be
// reached — oldest first. A message that does not get through stops the
// rest, to keep their order, and is tried again a little later.
func (f *Fleet) resend(ctx context.Context, name string, b Box, now time.Time) {
	mu := f.sendLock(name)
	if !mu.TryLock() {
		return // a send to it is under way, and sends what is pending first
	}
	defer mu.Unlock()
	f.mu.Lock()
	failed := f.resendFailed[name]
	f.mu.Unlock()
	if now.Sub(failed) < resendEvery {
		return
	}
	_ = f.flush(ctx, name, b, now)
}

// flush sends the receiver's pending messages, oldest first, each tried once,
// until one does not get through: its error is returned. One that has had
// resendTries is dropped instead, and you are told. The caller holds the
// receiver's send lock.
func (f *Fleet) flush(ctx context.Context, name string, b Box, now time.Time) error {
	pend, err := f.db.Pending(name, now.Add(-resendWindow))
	if err != nil {
		f.Log("ledger: pending relays to %s: %v", name, err)
		return nil
	}
	for _, r := range pend {
		note := resendNote
		if r.Error == ledger.Held {
			note = fmt.Sprintf(heldNote, r.At.UTC().Format("15:04 UTC"))
		}
		rctx, cancel := context.WithTimeout(ctx, f.holdAfter)
		err := b.Relay(rctx, name, r.From, note+r.Text)
		cancel()
		f.event(name, "fleet.resent", "", map[string]any{"relay": r.ID, "task": r.Task, "from": r.From, "delivered": err == nil, "held": r.Error == ledger.Held})
		f.mu.Lock()
		if err == nil {
			delete(f.resendTries, r.ID)
			delete(f.resendFailed, name)
		} else {
			f.resendTries[r.ID]++
			f.resendFailed[name] = now
		}
		tries := f.resendTries[r.ID]
		f.mu.Unlock()
		if err == nil {
			if e := f.db.EndRelay(r.ID, nil); e != nil {
				f.Log("ledger: end relay %d: %v", r.ID, e)
			}
			continue
		}
		if tries < resendTries {
			if r.Error == ledger.Interrupted {
				if e := f.db.HoldRelay(r.ID); e != nil { // tried again, like a held one
					f.Log("ledger: hold relay %d: %v", r.ID, e)
				}
			}
			return err
		}
		if e := f.db.EndRelay(r.ID, err); e != nil {
			f.Log("ledger: end relay %d: %v", r.ID, e)
		}
		f.mu.Lock()
		delete(f.resendTries, r.ID)
		f.mu.Unlock()
		what := "a message from " + r.From
		if r.Task != "" {
			what = "the message on task " + r.Task + " from " + r.From
		}
		text := fmt.Sprintf("%s to %s could not be delivered after %d tries (%v): send it again", what, name, tries, err)
		f.Log("%s", text)
		if f.notify != nil {
			if err := f.notify.Notify(ctx, text); err != nil {
				f.Log("alert %s: %v", name, err)
			}
		}
		f.event(name, "fleet.alert", "", map[string]string{"text": text})
	}
	return nil
}

// sendLock is the receiver's send lock (sendMus); a receiver fleetd does not
// know shares one.
func (f *Fleet) sendLock(name string) *sync.Mutex {
	if mu, ok := f.sendMus[name]; ok {
		return mu
	}
	return f.sendMus[""]
}

// execute runs one tick's actions. It reports false when an action that
// changes sessions failed, so the decision is retried rather than recorded.
func (f *Fleet) execute(ctx context.Context, name string, b Box, old, next reconcile.State, acts []reconcile.Action, now time.Time) bool {
	for _, a := range acts {
		var err error
		switch a.Kind {
		case reconcile.Start:
			err = b.Start(ctx)
			f.event(name, "fleet.container_start", "", map[string]string{"docker_error": a.Reason})
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
			if err = b.StopPID(ctx, a.PID); err == nil && m != nil && f.settle(ctx, name, b, old, a.Session, "", reconcile.ReasonStray, m, now) {
				err = b.DeleteTranscript(ctx, a.Session)
			}
			f.reap(ctx, name, b, a.Session)
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
			if f.notify != nil && f.firstAlert(a.Text, now) {
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
	f.reap(ctx, name, b, a.Session)
	if a.Task != "" && a.Reason != reconcile.ReasonTaskDone {
		_ = f.db.CloseTask(a.Task, now, a.Reason, "", "")
	}
	if m == nil {
		return nil
	}
	if !f.settle(ctx, name, b, st, a.Session, a.Task, a.Reason, m, now) {
		return nil // the transcript stays in the container; nothing is lost
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

// reap ends what a session that is gone left running: a job it detached, a
// server it started. Nothing of it belongs to the agent's next task.
func (f *Fleet) reap(ctx context.Context, name string, b Box, sid string) {
	if sid == "" {
		return
	}
	n, err := b.KillSessionProcs(ctx, sid)
	if err != nil {
		f.Log("reap %s/%s: %v", name, sid, err)
	}
	if n > 0 {
		f.event(name, "fleet.reaped", sid, map[string]int{"processes": n})
	}
}

// settle runs once a session is stopped, before its transcript is deleted: a
// message can land between the archive and the kill. If the transcript moved,
// it is archived again; if that cannot be proven, it reports false and the
// transcript is kept.
func (f *Fleet) settle(ctx context.Context, name string, b Box, st reconcile.State, sid, task, reason string, m *archive.Manifest, now time.Time) bool {
	h, err := b.HashTranscript(ctx, sid)
	if err != nil {
		return false
	}
	if h == m.SHA256 {
		return true
	}
	m2, err := f.archiveSession(ctx, name, b, st, sid, task, reason, now)
	if err != nil || m2 == nil {
		f.event(name, "fleet.late_archive_failed", sid, map[string]string{"error": fmt.Sprint(err)})
		return false
	}
	// The ledger now points at m2; m is the same transcript cut short.
	if ok, err := archive.Supersede(f.archiveRoot(), m.ID, m2.ID); err != nil || !ok {
		f.Log("%s: kept %s beside %s (superseded=%v): %v", name, m.ID, m2.ID, ok, err)
	}
	return true
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

// ErrNoTask is a wait on a task the ledger has never heard of.
var ErrNoTask = errors.New("no task")

func (f *Fleet) known(name string) error {
	if _, ok := f.cfg.Agents[name]; !ok {
		return fmt.Errorf("no agent named %q", name)
	}
	return nil
}

// Assign records that an agent was given a task. The orchestrator calls this
// (fleetctl assign) when it messages an agent with work.
func (f *Fleet) Assign(ctx context.Context, name, task, from, brief string, now time.Time) error {
	if err := f.known(name); err != nil {
		return err
	}
	if err := f.assign(name, task, from, now); err != nil {
		return err
	}
	if strings.TrimSpace(brief) == "" {
		return nil
	}
	return f.send(ctx, from, name, brief, task, now)
}

func (f *Fleet) assign(name, task, from string, now time.Time) error {
	f.locks[name].Lock()
	defer f.locks[name].Unlock()
	st := f.State(name)
	if st.Phase == reconcile.Disabled {
		return fmt.Errorf("%s is disabled (%s)", name, st.DisabledReason)
	}
	st.Task, st.Done, st.DoneAt, st.NudgedAt, st.LastBusy = task, false, time.Time{}, time.Time{}, now
	f.setState(st)
	// New work supersedes the agent's open tasks. Its session goes on (it is
	// still the agent's), so nothing is archived or rotated.
	old, err := f.db.SupersedeOpen(name, task, now)
	for _, id := range old {
		f.event(name, "fleet.superseded", st.SessionID, map[string]string{"task": id, "by": task})
	}
	if err != nil {
		return err
	}
	f.event(name, "fleet.assign", st.SessionID, map[string]string{"task": task, "from": from})
	return f.db.OpenTask(task, name, from, now)
}

// Done records a task's end; the agent's session is archived and replaced
// with a fresh one on the next idle tick. One task, one session.
func (f *Fleet) Done(name, task, outcome, branch, pr string, now time.Time) error {
	if name == "" {
		var err error
		if name, err = f.taskOwner(task); err != nil {
			return err
		}
	}
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
	st.Done, st.DoneAt = true, now
	if st.Phase == reconcile.Waiting {
		st.Phase = reconcile.Standby // the task is closed now; only the rotation waits for the idle tick
	}
	f.setState(st)
	f.event(name, "fleet.done", st.SessionID, map[string]string{"task": task, "outcome": outcome})
	if f.soonEvery > 0 {
		go f.rotateSoon(name)
	}
	if task == "" {
		return nil
	}
	if outcome == "" {
		outcome = "done"
	}
	return f.db.CloseTask(task, now, outcome, branch, pr)
}

// rotateSoon looks at an agent whose task is done every soonEvery, for two
// minutes or until it has rotated: done usually lands while the agent writes
// its last words, and the agent is out of the pool until its fresh session is
// up — the next tick could be thirty seconds away.
func (f *Fleet) rotateSoon(name string) {
	for i := 0; i < int(2*time.Minute/f.soonEvery); i++ {
		time.Sleep(f.soonEvery)
		if !f.State(name).Done {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*f.cfg.Thresholds.Tick)
		f.tickOne(ctx, name, time.Now())
		cancel()
	}
}

// Close records a task's end and leaves the agent's session alone: nothing is
// archived or rotated. For a task the agent has already moved on from, or
// one it should finish and keep its context after. Closing the agent's
// current task leaves it with none.
func (f *Fleet) Close(name, task, outcome, branch, pr string, now time.Time) error {
	if name == "" {
		var err error
		if name, err = f.taskOwner(task); err != nil {
			return err
		}
	}
	if err := f.known(name); err != nil {
		return err
	}
	f.locks[name].Lock()
	defer f.locks[name].Unlock()
	st := f.State(name)
	if task == "" {
		task = st.Task
	}
	if task == "" {
		return fmt.Errorf("%s has no open task to close (name one with --task)", name)
	}
	if outcome == "" {
		outcome = "done"
	}
	if st.Task == task {
		st.Task = ""
		if st.Phase == reconcile.Waiting {
			st.Phase = reconcile.Standby // its task is closed; nothing waits for the tick
		}
		f.setState(st)
	}
	f.event(name, "fleet.close", st.SessionID, map[string]string{"task": task, "outcome": outcome})
	return f.db.CloseTask(task, now, outcome, branch, pr)
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
	return f.TellKind(ctx, from, to, text, "", now)
}

// TellKind is Tell of a message of a kind (ledger.KindQuestion, KindProgress;
// "" is a report).
func (f *Fleet) TellKind(ctx context.Context, from, to, text, kind string, now time.Time) error {
	if !ledger.ValidKind(kind) {
		return fmt.Errorf("%w: kind %q: want report, question or progress", ErrBadInput, kind)
	}
	return f.deliverMsg(ctx, from, to, text, "", kind, now, true)
}

// inbox reports whether messages to a name are filed on the ledger, for it
// to read, instead of being relayed: a person, or a session that ran a task.
func (f *Fleet) inbox(name string) bool {
	if ledger.IsPerson(name) {
		return true
	}
	if _, ok := f.cfg.Agents[name]; ok || name == f.cfg.Orchestrator.Name {
		return false
	}
	return f.db.IsSession(name)
}

// noTask files a message under no task, whoever sent it: what fleetd says on
// an agent's behalf (an escalated question) is not the agent's report.
const noTask = "-"

// lateWindow is how long after a task closed its agent's messages still
// belong to it.
const lateWindow = 30 * time.Minute

// reportTask is the task an agent's message is filed under: its current one,
// or else its latest if that closed within lateWindow (a report after done or
// close is still the answer to that task).
func (f *Fleet) reportTask(agent string, now time.Time) string {
	if t := f.State(agent).Task; t != "" {
		return t
	}
	if t, err := f.db.LatestTask(agent); err == nil && t != nil &&
		(t.Closed.IsZero() || now.Sub(t.Closed) < lateWindow) {
		return t.ID
	}
	return ""
}

// Held is a message fleetd could not deliver now and keeps: it goes out when
// its receiver is back, after what was held for it before (resend). It is not
// a failure; the caller is told the message is queued.
type Held struct {
	To    string
	Cause error
}

func (h *Held) Error() string {
	return fmt.Sprintf("queued: %s cannot take it now (%v); fleetd delivers it when %s is back. Do not send it again.", h.To, h.Cause, h.To)
}

func (h *Held) Unwrap() error { return h.Cause }

// tell is Tell with the task an instruction belongs to: a brief, follow-up or
// cancel is filed under its task, like the agent's reports. With no task, an
// agent's own message is filed under its task. A message that does not get
// through is an error: a brief's run is placed again by its caller.
func (f *Fleet) tell(ctx context.Context, from, to, text, task string, now time.Time) error {
	return f.deliverMsg(ctx, from, to, text, task, "", now, false)
}

// send is tell for a message nobody would send again: a follow-up, a cancel
// notice, a message between sessions. When its receiver cannot take it — its
// host does not answer, it is down, or the relay did not get through in
// holdAfter — it is held (*Held) and goes out when the receiver is back.
func (f *Fleet) send(ctx context.Context, from, to, text, task string, now time.Time) error {
	return f.deliverMsg(ctx, from, to, text, task, "", now, true)
}

// unable says why a receiver cannot take a message now, without trying: its
// host does not answer, or its container is down. nil when it may.
func (f *Fleet) unable(to string) error {
	a, ok := f.cfg.Agents[to]
	if !ok {
		return nil
	}
	switch st := f.State(to); {
	case !st.UnreachableSince.IsZero():
		return fmt.Errorf("its host %s does not answer", a.Host)
	case st.Phase == reconcile.Down:
		return fmt.Errorf("its container is down")
	}
	return nil
}

func (f *Fleet) deliverMsg(ctx context.Context, from, to, text, task, kind string, now time.Time, hold bool) error {
	text = f.redactSecrets(text)
	// The receiver is told who sent it and replies there, so the sender must
	// be someone a reply can reach (or a person, who reads the ledger).
	if from != f.cfg.Orchestrator.Name && !f.inbox(from) {
		if err := f.known(from); err != nil {
			return fmt.Errorf("unknown sender: %w", err)
		}
	}
	// What an agent writes while it has a task is filed under that task (done
	// does not clear it: a late report still belongs). Nobody else's does,
	// and neither does a word to another agent that is not the assigner: that
	// is a peer's chat, not a report on the task.
	if _, ok := f.cfg.Agents[from]; ok && task == "" {
		task = f.reportTask(from, now)
		if _, toAgent := f.cfg.Agents[to]; toAgent && task != "" {
			if t, err := f.db.Task(task); err != nil || t.Assigner != to {
				task = ""
			}
		}
	}
	if task == noTask {
		task = ""
	}
	// A person's or a session's inbox: stored, read with Inbox and Wait.
	if f.inbox(to) {
		if f.inbox(from) && ledger.IsPerson(from) && ledger.IsPerson(to) {
			return fmt.Errorf("%s does not message %s through the fleet: people read the ledger", from, to)
		}
		if err := f.db.AddRelay(ledger.Relay{From: from, To: to, Task: task, Text: text, Kind: kind, At: now, Delivered: true}); err != nil {
			return err
		}
		f.event(from, "fleet.report", "", map[string]any{"to": to, "size": len(text)})
		return nil
	}
	if to != f.cfg.Orchestrator.Name {
		if err := f.known(to); err != nil {
			return err
		}
	}
	// On the ledger before it is sent: a fleetd that dies sending it leaves it
	// there, and the next one sends it again (resend).
	id, berr := f.db.BeginRelay(ledger.Relay{From: from, To: to, Task: task, Text: text, Kind: kind, At: now})
	if berr != nil {
		f.Log("ledger: relay to %s: %v", to, berr)
	}
	var err error
	if hold && berr == nil {
		// In order: what is held for the receiver goes first, and while it
		// cannot go, this waits behind it.
		mu := f.sendLock(to)
		mu.Lock()
		defer mu.Unlock()
		b := f.box(to)
		if err = f.unable(to); err == nil {
			err = f.flush(ctx, to, b, now)
		}
		if err == nil {
			rctx, cancel := context.WithTimeout(ctx, f.holdAfter)
			err = f.relay(rctx, b, to, from, text)
			cancel()
		}
		if err != nil {
			if e := f.db.HoldRelay(id); e != nil {
				f.Log("ledger: hold relay %d: %v", id, e)
				return err
			}
			f.event(to, "fleet.relay", "", map[string]any{"from": from, "task": task, "delivered": false, "held": true, "size": len(text), "error": err.Error()})
			return &Held{To: to, Cause: err}
		}
	} else {
		err = f.relay(ctx, f.box(to), to, from, text)
	}
	if berr == nil {
		if e := f.db.EndRelay(id, err); e != nil {
			f.Log("ledger: end relay %d: %v", id, e)
		}
	}
	f.event(to, "fleet.relay", "", map[string]any{"from": from, "task": task, "delivered": err == nil, "size": len(text)})
	return err
}

// relay sends a message, and tries again (relayBackoff) when it did not get
// through: a session that is restarting or rotating has nobody to take it for
// a few seconds.
func (f *Fleet) relay(ctx context.Context, b Box, to, from, text string) error {
	err := b.Relay(ctx, to, from, text)
	for _, d := range f.relayBackoff {
		if err == nil || ctx.Err() != nil {
			break
		}
		f.Log("%v; trying again in %s", err, d)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(d):
		}
		err = b.Relay(ctx, to, from, text)
	}
	return err
}

// Inbox is the messages agents and the orchestrator sent the operator,
// after the given id.
func (f *Fleet) Inbox(after int64) ([]ledger.Relay, error) { return f.db.Inbox(after) }

// InboxOf is Inbox of a session's, or a person's: what was sent to them.
func (f *Fleet) InboxOf(to string, after int64) ([]ledger.Relay, error) {
	return f.db.InboxOf(to, after)
}

// WaitResult is why a Wait returned: "message" (the agent wrote to the
// operator), "closed" (its task closed), "phase" (it went down, cooling or
// disabled) or "timeout" (the context ended).
type WaitResult struct {
	Reason  string        `json:"reason"`
	Message *ledger.Relay `json:"message,omitempty"`
	Phase   string        `json:"phase,omitempty"`
	Task    *ledger.Task  `json:"task,omitempty"`
}

// Wait blocks until the agent messages the operator (after the inbox id
// after), its task closes, or it can no longer work, whichever is first.
// With a task named, the agent may be left out (it is the task's), and a
// message is anything filed under the task after the relay id after, whoever
// it was sent to.
func (f *Fleet) Wait(ctx context.Context, agent, task string, after int64) (WaitResult, error) {
	return f.wait(ctx, agent, task, after, "")
}

// WaitNew is Wait for the messages a reader — a session, by name — has not
// been handed yet: it returns the oldest message filed under the task after
// the reader's cursor, and moves the cursor past it. Called again it returns
// the next one, and never the same twice. The task is required.
func (f *Fleet) WaitNew(ctx context.Context, agent, task, reader string) (WaitResult, error) {
	if task == "" || reader == "" {
		return WaitResult{}, fmt.Errorf("%w: wait --new needs a task and a reader", ErrBadInput)
	}
	return f.wait(ctx, agent, task, 0, reader)
}

func (f *Fleet) wait(ctx context.Context, agent, task string, after int64, reader string) (WaitResult, error) {
	if agent == "" && task == "" {
		return WaitResult{}, fmt.Errorf("wait needs an agent or a task")
	}
	if task != "" {
		// A task nobody assigned would never close: say so now, not at the timeout.
		tk, err := f.TaskAgent(task)
		if err != nil {
			return WaitResult{}, err
		}
		if agent == "" {
			agent = tk.Agent
		}
	}
	if err := f.known(agent); err != nil {
		return WaitResult{}, err
	}
	if reader != "" {
		var err error
		if after, err = f.db.Cursor(task, reader); err != nil {
			return WaitResult{}, err
		}
	} else if task != "" && after == 0 {
		// "The answer to my latest instruction": what the agent wrote before
		// the last brief or follow-up answered something older.
		var err error
		if after, err = f.db.LastInstruction(task); err != nil {
			return WaitResult{}, err
		}
	}
	t := time.NewTicker(f.waitEvery)
	defer t.Stop()
	for {
		var msgs []ledger.Relay
		var err error
		if task != "" {
			msgs, err = f.db.TaskReports(task, after)
		} else {
			msgs, err = f.db.Inbox(after)
		}
		if err != nil {
			return WaitResult{}, err
		}
		for i := range msgs {
			if task != "" || msgs[i].From == agent {
				if reader != "" {
					if err := f.db.SetCursor(task, reader, msgs[i].ID); err != nil {
						return WaitResult{}, err
					}
				}
				return WaitResult{Reason: "message", Message: &msgs[i]}, nil
			}
		}
		if len(msgs) > 0 {
			after = msgs[len(msgs)-1].ID
		}
		if task != "" {
			if tk, err := f.db.Task(task); err == nil && !tk.Closed.IsZero() {
				return WaitResult{Reason: "closed", Task: tk}, nil
			}
		}
		st := f.State(agent)
		switch p := st.Phase; p {
		case reconcile.Down, reconcile.CodeMissing, reconcile.Cooling, reconcile.Disabled:
			if !st.Recovering(time.Now()) { // a power cut's start-up fleetd is fixing is no reason to give up
				return WaitResult{Reason: "phase", Phase: string(p)}, nil
			}
		}
		if st.OutOfReach(st.RecordedAt) {
			return WaitResult{Reason: "phase", Phase: string(reconcile.Unreachable)}, nil
		}
		select {
		case <-ctx.Done():
			return WaitResult{Reason: "timeout"}, nil
		case <-t.C:
		}
	}
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
	if cur == m.SessionID {
		// Its transcript in the container is this archive plus whatever came
		// after; restoring would overwrite that with the older copy.
		return fmt.Errorf("%s is already running session %s; nothing to restore", name, cur)
	}
	if st.Phase == reconcile.Working {
		return fmt.Errorf("%s is mid-turn; restore when it is idle", name)
	}
	var curM *archive.Manifest
	if cur != "" {
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
	st.TurnOpen = false // that turn was the replaced session's
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
	IdleS       int64     `json:"idle_s"`
	Model       string    `json:"model,omitempty"`
	Alerts      []string  `json:"alerts"`
	RecordedAt  time.Time `json:"recorded_at,omitzero"`
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
	// Unreachable: its host did not answer the last tick.
	Unreachable bool `json:"unreachable,omitempty"`
	// Rotating: its task is done, and its fresh session is not up yet.
	Rotating bool `json:"rotating,omitempty"`
}

// activeAlerts is the alerts fired and not yet cleared: reconcile keeps a
// key in Alerted for exactly as long as its episode lasts.
func activeAlerts(st reconcile.State) []string {
	out := []string{}
	for k, on := range st.Alerted {
		if on {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Status is the roster, one row per agent, sorted.
func (f *Fleet) Status(now time.Time) []Row {
	var rows []Row
	for _, name := range f.cfg.AgentNames() {
		st, a := f.State(name), f.cfg.Agents[name]
		idle, idleS := "", int64(0)
		if !st.LastBusy.IsZero() && st.Phase != reconcile.Working {
			idle, idleS = Dur(now.Sub(st.LastBusy)), int64(now.Sub(st.LastBusy).Seconds())
		}
		phase, task := st.Phase, st.Task
		if st.Done { // closed: the archive and fresh session wait for the idle tick, the task does not
			task = ""
			if phase == reconcile.Waiting {
				phase = reconcile.Standby
			}
		}
		if task != "" && phase == reconcile.Standby {
			phase = reconcile.Waiting // assigned since the last tick
		}
		if st.OutOfReach(now) {
			phase = reconcile.Unreachable // shown, not stored: the phase is kept for its return
		}
		rows = append(rows, Row{Agent: name, Host: a.Host, Account: a.Account, State: string(phase), Task: task,
			Idle: idle, IdleS: idleS, Model: st.Model, Alerts: activeAlerts(st), RecordedAt: st.RecordedAt, FiveHour: st.Quota.FiveHour, SevenDay: st.Quota.SevenDay, FiveResets: st.Quota.FiveHourResets,
			SevenResets: st.Quota.SevenDayResets, QuotaAt: st.Quota.At, Context: st.ContextPct, Low: st.Low,
			Reserved: st.Reserved, Models: a.Models, Session: st.SessionID, Reason: st.DisabledReason, Cooling: st.CoolingUntil,
			Unreachable: !st.UnreachableSince.IsZero(), Rotating: st.Done})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Agent < rows[j].Agent })
	return rows
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
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
