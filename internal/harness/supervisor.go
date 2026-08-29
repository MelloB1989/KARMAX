package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Policy is how one kind of session behaves. Everything here is configuration,
// so a new use-case adds a kind and never edits this package.
type Policy struct {
	Model       string
	Idle        time.Duration
	MaxTurns    int
	TurnTimeout time.Duration
	MaxCostUSD  float64
	Ephemeral   bool
}

// Store is what the supervisor needs to remember sessions across restarts.
type Store interface {
	SaveHarnessSession(h SessionRecord) error
	RecordHarnessTurn(key string, costUSD float64, in, out, cacheRead int64, at time.Time) error
	SetHarnessState(key, state, lastErr string, at time.Time) error
	GetHarnessSession(key string) (*SessionRecord, error)
	ListHarnessSessions(states ...string) ([]SessionRecord, error)
	DeleteHarnessSession(key string) error
}

// Session states, mirroring the store's own vocabulary.
const (
	HarnessStarting = "starting"
	HarnessLive     = "live"
	HarnessIdle     = "idle"
	HarnessDead     = "dead"
	HarnessClosed   = "closed"
)

// SessionRecord mirrors the stored row, kept here so this package does not
// depend on the store's concrete types.
type SessionRecord struct {
	Key              string
	HarnessSessionID string
	Kind             string
	Model            string
	PID              int
	State            string
	Workdir          string
	StartedAt        time.Time
	LastActivityAt   time.Time
	Turns            int
	CostUSD          float64
	InputTokens      int64
	OutputTokens     int64
	CacheRead        int64
	LastError        string
}

// Config parameterises the supervisor.
type Config struct {
	Binary      string
	WorkdirRoot string
	MaxLive     int
	Policies    map[string]Policy
	Env         []string
	Allowlist   map[string]bool
}

// Supervisor owns every live harness session.
type Supervisor struct {
	cfg     Config
	store   Store
	breaker *Breaker
	log     Logger
	audit   func(sessionKey string, tc ToolCall, allowed bool)

	mu   sync.Mutex
	live map[string]*Session
}

// Logger is the small slice of logging this package needs.
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
}

func New(cfg Config, st Store, br *Breaker, log Logger, audit func(string, ToolCall, bool)) *Supervisor {
	if cfg.MaxLive <= 0 {
		cfg.MaxLive = 6
	}
	if cfg.Binary == "" {
		cfg.Binary = "claude"
	}
	return &Supervisor{
		cfg: cfg, store: st, breaker: br, log: log, audit: audit,
		live: map[string]*Session{},
	}
}

// ErrBreakerOpen is returned when quota policy forbids a harness call, so the
// caller can fall back rather than fail.
type ErrBreakerOpen struct{ Reason string }

func (e ErrBreakerOpen) Error() string { return "harness unavailable: " + e.Reason }

// Options let a workflow shape its own session without core knowing why.
//
// Both fields exist because a harness reads its environment from the directory
// it runs in: CLAUDE.md files are loaded from the working directory AND every
// parent, and they all merge. So a workflow that wants its sessions to know who
// they work for, which tools to reach for and what to remember writes that file
// and points its sessions at that directory — no core change, no core knowledge
// of the use-case.
type Options struct {
	// Workdir overrides where the session runs. Empty uses the default under
	// the supervisor's root.
	Workdir string
	// Instructions is written to CLAUDE.md in the workdir before the first
	// spawn. Rewritten when it changes, so a workflow can evolve its own
	// standing instructions without restarting anything.
	Instructions string
}

// Send is the whole caller-facing surface: give it a key and a message.
//
// Opening, resuming and reaping are consequences of this call, not separate
// things a caller has to remember to do.
func (s *Supervisor) Send(ctx context.Context, key, kind, text string) (Turn, error) {
	return s.SendWith(ctx, key, kind, text, Options{})
}

// SendWith is Send with the workflow's own working directory and standing
// instructions.
func (s *Supervisor) SendWith(ctx context.Context, key, kind, text string, opt Options) (Turn, error) {
	if ok, why := s.breaker.Allow(); !ok {
		return Turn{}, ErrBreakerOpen{Reason: why}
	}
	pol := s.policy(kind)

	sess, err := s.open(ctx, key, kind, pol, opt)
	if err != nil {
		return Turn{}, err
	}

	turn, err := sess.Send(ctx, text, pol.TurnTimeout)

	// Quota is reported per turn, so the breaker learns from every call
	// including the ones that fail.
	s.breaker.Observe(turn.Limits)

	for _, tc := range turn.ToolCalls {
		allowed := tc.Command == "" || s.cfg.Allowlist[tc.Command]
		if s.audit != nil {
			s.audit(key, tc, allowed)
		}
	}

	now := time.Now()
	if err != nil {
		// A failed turn leaves a process that may still be mid-thought. Drop it
		// and let the next call resume the transcript instead.
		s.kill(key, HarnessDead, err.Error())
		return turn, err
	}

	_ = s.store.RecordHarnessTurn(key, turn.CostUSD, turn.Usage.InputTokens,
		turn.Usage.OutputTokens, turn.Usage.CacheReadTokens, now)

	// Per-session circuit breakers. One runaway conversation must not be able
	// to drain a window that the whole account shares.
	if rec, _ := s.store.GetHarnessSession(key); rec != nil {
		if pol.MaxTurns > 0 && rec.Turns >= pol.MaxTurns {
			s.log.Warn("harness: closing a session at its turn limit", "key", key, "turns", rec.Turns)
			s.Close(key)
		} else if pol.MaxCostUSD > 0 && rec.CostUSD >= pol.MaxCostUSD {
			s.log.Warn("harness: closing a session at its cost limit", "key", key, "cost_usd", rec.CostUSD)
			s.Close(key)
		}
	}
	return turn, nil
}

// open returns a usable session, reusing, resuming or creating in that order.
//
// The order is the crash-safety story. A live process is reused; a dead one
// whose transcript we know is resumed, which brings its context back; only a
// genuinely new key starts cold.
func (s *Supervisor) open(ctx context.Context, key, kind string, pol Policy, opt Options) (*Session, error) {
	s.mu.Lock()
	if sess, ok := s.live[key]; ok && sess.Alive() {
		s.mu.Unlock()
		return sess, nil
	}
	delete(s.live, key)
	s.mu.Unlock()

	rec, err := s.store.GetHarnessSession(key)
	if err != nil {
		return nil, err
	}

	resume := rec != nil && rec.HarnessSessionID != ""
	id := ""
	if resume {
		id = rec.HarnessSessionID
	} else {
		id = uuid.New().String()
	}

	model := pol.Model
	workdir := strings.TrimSpace(opt.Workdir)
	if workdir == "" {
		workdir = filepath.Join(s.cfg.WorkdirRoot, sanitize(key))
	}
	sess := &Session{Key: key, Kind: kind, ID: id, Model: model}

	// Written BEFORE the spawn. A crash in between leaves a row the startup
	// sweep can find; the reverse leaves a process nothing knows about.
	now := time.Now()
	started := now
	if rec != nil {
		started = rec.StartedAt
	}
	if err := s.store.SaveHarnessSession(SessionRecord{
		Key: key, HarnessSessionID: id, Kind: kind, Model: model,
		State: HarnessStarting, Workdir: workdir,
		StartedAt: started, LastActivityAt: now,
	}); err != nil {
		return nil, err
	}

	s.evictIfFull()

	// Written before the spawn, because the harness reads it as it starts.
	if err := writeInstructions(workdir, opt.Instructions); err != nil {
		s.log.Warn("harness: could not write session instructions",
			"key", key, "workdir", workdir, "err", err.Error())
	}

	if err := spawn(ctx, s.cfg.Binary, sess, workdir, resume, s.cfg.Env); err != nil {
		s.breaker.TripOn(fmt.Sprintf("could not start %s: %v", s.cfg.Binary, err))
		_ = s.store.SetHarnessState(key, HarnessDead, err.Error(), time.Now())
		return nil, err
	}

	_ = s.store.SaveHarnessSession(SessionRecord{
		Key: key, HarnessSessionID: sess.ID, Kind: kind, Model: model,
		PID: sess.PID(), State: HarnessLive, Workdir: workdir,
		StartedAt: started, LastActivityAt: time.Now(),
	})

	s.mu.Lock()
	s.live[key] = sess
	s.mu.Unlock()

	s.log.Info("harness: session open", "key", key, "kind", kind,
		"model", model, "resumed", resume, "pid", sess.PID())
	return sess, nil
}

// evictIfFull makes room by closing the least recently used session.
//
// Without a cap, one busy chat spawns processes until the machine gives out.
func (s *Supervisor) evictIfFull() {
	s.mu.Lock()
	over := len(s.live) >= s.cfg.MaxLive
	s.mu.Unlock()
	if !over {
		return
	}
	recs, err := s.store.ListHarnessSessions(HarnessLive, HarnessIdle)
	if err != nil || len(recs) == 0 {
		return
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].LastActivityAt.Before(recs[j].LastActivityAt) })
	s.log.Info("harness: at the session cap, evicting the least recently used", "key", recs[0].Key)
	s.Close(recs[0].Key)
}

// Close ends a session. Ephemeral kinds forget it entirely.
func (s *Supervisor) Close(key string) {
	s.mu.Lock()
	sess := s.live[key]
	delete(s.live, key)
	s.mu.Unlock()

	if sess != nil {
		sess.Close()
	}
	rec, _ := s.store.GetHarnessSession(key)
	if rec != nil && s.policy(rec.Kind).Ephemeral {
		_ = s.store.DeleteHarnessSession(key)
		if rec.Workdir != "" {
			_ = os.RemoveAll(rec.Workdir)
		}
		return
	}
	_ = s.store.SetHarnessState(key, HarnessClosed, "", time.Now())
}

func (s *Supervisor) kill(key, state, reason string) {
	s.mu.Lock()
	sess := s.live[key]
	delete(s.live, key)
	s.mu.Unlock()
	if sess != nil {
		sess.Close()
	}
	_ = s.store.SetHarnessState(key, state, reason, time.Now())
}

// Reap closes sessions that have gone quiet past their kind's idle window.
func (s *Supervisor) Reap(now time.Time) {
	recs, err := s.store.ListHarnessSessions(HarnessLive, HarnessIdle)
	if err != nil {
		return
	}
	for _, r := range recs {
		pol := s.policy(r.Kind)
		if pol.Idle <= 0 {
			continue
		}
		if now.Sub(r.LastActivityAt) > pol.Idle {
			s.log.Info("harness: closing an idle session", "key", r.Key,
				"idle_for", now.Sub(r.LastActivityAt).Round(time.Second).String())
			s.Close(r.Key)
		}
	}
}

// ReapOrphans runs at startup, when every pid in the table belongs to a process
// this daemon no longer owns.
//
// The rows are marked dead rather than deleted: the transcript is still there,
// so the next message resumes the conversation instead of starting one.
func (s *Supervisor) ReapOrphans() {
	recs, err := s.store.ListHarnessSessions(HarnessStarting, HarnessLive, HarnessIdle)
	if err != nil {
		return
	}
	for _, r := range recs {
		if r.PID > 0 {
			if p, err := os.FindProcess(r.PID); err == nil {
				_ = p.Kill() // best effort: it is not ours to talk to any more
			}
		}
		_ = s.store.SetHarnessState(r.Key, HarnessDead, "daemon restarted", time.Now())
	}
	if len(recs) > 0 {
		s.log.Info("harness: marked sessions dead after a restart", "count", len(recs))
	}
}

// Live reports the keys with a running process, for the CLI.
func (s *Supervisor) Live() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.live))
	for k, sess := range s.live {
		if sess.Alive() {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Shutdown closes every live session, so a daemon stop does not leak processes.
func (s *Supervisor) Shutdown() {
	for _, k := range s.Live() {
		s.Close(k)
	}
}

func (s *Supervisor) policy(kind string) Policy {
	if p, ok := s.cfg.Policies[kind]; ok {
		return withDefaults(p)
	}
	return withDefaults(Policy{})
}

func withDefaults(p Policy) Policy {
	if p.TurnTimeout <= 0 {
		p.TurnTimeout = 2 * time.Minute
	}
	if p.Idle <= 0 {
		p.Idle = 10 * time.Minute
	}
	return p
}

// sanitize turns a session key into a directory name.
func sanitize(key string) string {
	out := make([]rune, 0, len(key))
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// writeInstructions puts a workflow's standing instructions where the harness
// will read them.
//
// CLAUDE.md is loaded from the working directory and every parent, and they
// merge — verified: a file three levels up still applied alongside the nearest
// one. So a shared file at the sessions root can carry what every session needs
// and this one carries only what is particular to the workflow.
//
// Rewritten only when the content differs, so a session that resumes into an
// unchanged directory does not see its instructions churn.
func writeInstructions(workdir, content string) error {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(workdir, "CLAUDE.md")
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return nil
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
