package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/browser"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"go.uber.org/zap"
)

// harnessStore adapts the store to what the supervisor needs, so the harness
// package carries no dependency on the database's concrete types.
type harnessStore struct{ s *store.Store }

func (h harnessStore) SaveHarnessSession(r harness.SessionRecord) error {
	return h.s.SaveHarnessSession(store.HarnessSession(r))
}
func (h harnessStore) RecordHarnessTurn(key string, cost float64, in, out, cr int64, at time.Time) error {
	return h.s.RecordHarnessTurn(key, cost, in, out, cr, at)
}
func (h harnessStore) SetHarnessState(key, state, lastErr string, at time.Time) error {
	return h.s.SetHarnessState(key, state, lastErr, at)
}
func (h harnessStore) GetHarnessSession(key string) (*harness.SessionRecord, error) {
	got, err := h.s.GetHarnessSession(key)
	if err != nil || got == nil {
		return nil, err
	}
	r := harness.SessionRecord(*got)
	return &r, nil
}
func (h harnessStore) ListHarnessSessions(states ...string) ([]harness.SessionRecord, error) {
	rows, err := h.s.ListHarnessSessions(states...)
	if err != nil {
		return nil, err
	}
	out := make([]harness.SessionRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, harness.SessionRecord(r))
	}
	return out, nil
}
func (h harnessStore) DeleteHarnessSession(key string) error {
	return h.s.DeleteHarnessSession(key)
}

// harnessLog adapts zap to the supervisor's small logging interface.
type harnessLog struct{ l *zap.Logger }

func (h harnessLog) Info(msg string, kv ...any) { h.l.Info(msg, sugar(kv)...) }
func (h harnessLog) Warn(msg string, kv ...any) { h.l.Warn(msg, sugar(kv)...) }

func sugar(kv []any) []zap.Field {
	out := make([]zap.Field, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, zap.Any(fmt.Sprint(kv[i]), kv[i+1]))
	}
	return out
}

// startHarness builds the supervisor, or returns nil when the feature is off.
//
// Nil is a supported value everywhere it is used: the harness is an optional
// engine, and every caller already has an API path to fall back to.
func (rt *KarmaxRuntime) startHarness() *harness.Supervisor {
	hc := rt.cfg.Harness
	if !hc.Enabled {
		return nil
	}

	root := hc.WorkdirRoot
	if root == "" {
		root = filepath.Join(hostDataDir(), "sessions")
	}
	allow := map[string]bool{}
	for _, c := range hc.Allowlist {
		allow[strings.TrimSpace(c)] = true
	}

	policies := map[string]harness.Policy{}
	for name, k := range hc.Kinds {
		policies[name] = harness.Policy{
			Model:       k.Model,
			Idle:        parseDur(k.Idle, 10*time.Minute),
			MaxTurns:    k.MaxTurns,
			TurnTimeout: parseDur(k.TurnTimeout, 2*time.Minute),
			MaxCostUSD:  k.MaxCostUSD,
			Ephemeral:   k.Ephemeral,
		}
	}

	// The breaker announces both directions. A limiter that trips quietly is
	// indistinguishable from a feature nobody uses.
	breaker := harness.NewBreaker(hc.WindowShare, func(tripped bool, reason string) {
		if tripped {
			rt.log.Warn("harness: quota state changed", zap.String("reason", reason))
			builtin.PushAppNotification(rt.store, "", "alert",
				"Claude quota is tight", reason)
			return
		}
		rt.log.Info("harness: back to the full model", zap.String("was", reason))
		builtin.PushAppNotification(rt.store, "", "info",
			"Claude quota recovered", "The rate-limit window rolled; KARMAX is back on its usual model.")
	})

	// Defaulted here rather than left empty, because an empty cheap tier turns
	// the degrade into a no-op and the first busy afternoon spends the whole
	// window on the good model.
	cheap := strings.TrimSpace(hc.CheapModel)
	if cheap == "" {
		cheap = "haiku"
	}

	sup := harness.New(harness.Config{
		Binary:        hc.Binary,
		WorkdirRoot:   root,
		MaxLive:       hc.MaxLive,
		Policies:      policies,
		Env:           harnessEnviron(),
		Allowlist:     allow,
		CheapModel:    cheap,
		FallbackModel: strings.TrimSpace(hc.FallbackModel),
	}, harnessStore{rt.store}, breaker, harnessLog{rt.log}, rt.auditHarnessTool)

	// The brief every session inherits, at the DATA ROOT rather than the
	// sessions directory. CLAUDE.md merges upward from the working directory,
	// so putting it here reaches every session wherever a workflow chooses to
	// run one — including wa-sessions/, which is not under the default root.
	if err := writeRootBrief(hostDataDir()); err != nil {
		rt.log.Warn("harness: could not write the shared session brief", zap.Error(err))
	}

	// browserMCPCache wraps the same shared session browserMCPConfig used to
	// probe directly, so chatTurn and harnessSenderFor.Send get it cached.
	rt.browserMCPCache = newBrowserMCPCache(browser.Shared(rt.cfg.Karmax.DataDir))

	// --mcp-config is fixed when a warm session spawns, so a session outlives
	// the browser state that justified it. Recycling on start and stop is how
	// the next turn gets a process with the current flags; invalidating the
	// cache on the same signal is how it gets a current --mcp-config to spawn
	// with in the first place.
	browser.Shared(rt.cfg.Karmax.DataDir).OnStateChange(rt.onBrowserStateChange)

	// Every pid in the table belongs to a process this daemon no longer owns.
	sup.ReapOrphans()

	rt.harnessBreaker = breaker
	return sup
}

// auditHarnessTool records what a session did.
//
// Sessions run with a real shell, so this cannot prevent a bad action. It makes
// one impossible to miss, which is the honest description of what an audit is.
func (rt *KarmaxRuntime) auditHarnessTool(sessionKey string, tc harness.ToolCall, allowed bool) {
	rt.bus.Publish(bus.NewEvent(bus.EventToolCalled, "", map[string]any{
		"tool":    "harness:" + tc.Name,
		"session": sessionKey,
		"command": tc.Command,
		"allowed": allowed,
	}))
	if allowed {
		return
	}
	rt.log.Warn("harness ran a command outside the allowlist",
		zap.String("session", sessionKey), zap.String("command", tc.Command))
	builtin.PushAppNotification(rt.store, "", "alert",
		"Harness ran something unexpected",
		fmt.Sprintf("Session %s ran %q, which is not on the allowlist.", sessionKey, tc.Command))
}

// harnessEnviron strips KARMAX's own model credentials from a session.
//
// The harness authenticates as itself; leaking the daemon's provider keys in
// would let it bill the operator's metered account instead of the subscription
// the whole design is trying to use.
func harnessEnviron() []string {
	drop := map[string]bool{
		"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true,
		"ANTHROPIC_BASE_URL": true, "AZURE_OPENAI_API_KEY": true,
		"AZURE_OPENAI_BASE_URL": true, "OPENAI_API_KEY": true,
		"KARMA_ANTHROPIC_BEDROCK": true,
	}
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && drop[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func parseDur(s string, def time.Duration) time.Duration {
	if strings.TrimSpace(s) == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return def
	}
	return d
}

func hostDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".karmax")
}

var _ = config.HarnessConfig{}

// startHarnessReaper closes sessions that have gone quiet.
//
// The idle window is the whole economics of this design: a session held open
// costs nothing but a process, while one closed too eagerly pays ~12.7k tokens
// of cold-start overhead the next time somebody speaks.
func (rt *KarmaxRuntime) startHarnessReaper(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				rt.harness.Shutdown() // never leave processes behind
				return
			case now := <-t.C:
				rt.harness.Reap(now)
			}
		}
	}()
}

// writeRootBrief puts the instructions every harness session inherits at the
// root of the sessions directory.
//
// CLAUDE.md is read from the working directory and from every parent, and they
// merge — so this file reaches every session without being copied into each
// one, and a workflow's own file adds to it rather than replacing it.
//
// It says nothing about any particular integration. Whose assistant a session
// is, and what it may do on someone's behalf, is the workflow's to state.
func writeRootBrief(root string) error { //nolint:revive // root is the data dir
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	path := filepath.Join(root, "CLAUDE.md")
	if old, err := os.ReadFile(path); err == nil && string(old) == rootBrief {
		return nil
	}
	return os.WriteFile(path, []byte(rootBrief), 0o644)
}

const rootBrief = `# You are running inside KARMAX

KARMAX is your operator's always-on assistant. It handles memory, events, comms,
scheduling and automation; you are the part that thinks. A workflow started this
session for a specific job — read the CLAUDE.md beside this one for what that is.

## Reaching KARMAX

Everything KARMAX can do is a shell call away. There is no API to learn:

    karmax tool list                     # every tool, with its arguments
    karmax tool call <name> k=v k=v      # invoke one
    karmax tool call <name> --json '{…}' # when a value is not a simple string

Run ` + "`karmax tool list`" + ` when you are unsure what exists. Guessing a tool name
wastes a turn; the list is authoritative and cheap.

## Memory — query it, and keep it

Long-term memory is GitLoom, shared with the rest of KARMAX. It is the difference
between an assistant and a chatbot, and it only works if you use it in both
directions.

Before answering anything that refers to a person, a project, a deal or a
decision, look it up:

    karmax memory search "<what you need to know>"
    karmax tool call memory.retrieve query="<a harder question, multi-step>"

After anything durable happens — a decision, a commitment, a deadline, a fact
about someone, a changed status — write it down:

    karmax tool call memory.ingest content="<one standalone fact>" category=<people|projects|decisions|context> importance=<low|medium|high|critical>

Rules that keep memory usable rather than merely large:

- ONE fact per ingest, phrased so it stands alone months later. Not a transcript,
  not a summary of a conversation, not "he said ok".
- Include WHO said it when that matters. A third party's request recorded as the
  operator's instruction becomes a standing order nobody gave.
- When a fact CHANGES, ingest the corrected version and retire the old one with
  ` + "`karmax tool call memory.forget id=<path>`" + `. A stale fact left beside its
  correction will be retrieved instead of it.
- Do not re-derive a list you already store. Update the existing fact.

## Honesty

- Never say something is done unless a command in THIS turn did it.
- Never state what a message said, who sent it, or when, unless it is in this
  turn's output. "I could not find it" always beats a plausible invention.
- If a tool fails, say so plainly and say what you tried.

## Acting

You have a real shell and real tools. Prefer doing the thing to describing it,
and prefer one clear question to a menu of options when you genuinely cannot
proceed. Do not narrate what you are about to do and then stop.
`

// browserKinds is an allowlist, not a "skip these" list: a kind not named
// here gets no browser, so a kind added later (utility included, whenever the
// categories plan lands) stays browser-less without this file changing.
var browserKinds = map[string]bool{
	"chat":  true,
	"agent": true,
}

// harnessRecycler is what recycling idle browser-taking sessions needs from
// the supervisor, small enough to fake in tests.
type harnessRecycler interface {
	Live() []string
	// CloseIfIdle closes key only if it is not busy, checking and removing it
	// from the live table atomically — a separate Busy-then-Close here would
	// reopen the exact TOCTOU CloseIfIdle exists to close: the session can
	// become busy in the gap between the two calls, and Close does not
	// re-check.
	CloseIfIdle(key string) bool
}

// recycleIdleBrowserSessions closes idle sessions of the kinds that take a
// browser, so the next turn respawns one with --mcp-config matching whatever
// the browser just became. A busy session is left alone: closing it mid-turn
// would kill a running answer in front of the operator — CloseIfIdle is what
// guarantees that atomically rather than as two calls a scheduler can split.
//
// Each close runs on its own goroutine: a stubborn process can take up to 3s
// to give up its SIGKILL fallback, and with MaxLive sessions to consider,
// closing them one at a time would make one recycling pass take minutes
// instead of seconds. The caller (onBrowserStateChange) already runs this
// off the browser's own Start/Stop call path, but a slow pass still delays
// the log line and leaves stale processes around longer than it has to.
func recycleIdleBrowserSessions(sup harnessRecycler, kindOf map[string]string, kinds map[string]bool) {
	var wg sync.WaitGroup
	for _, key := range sup.Live() {
		if !kinds[kindOf[key]] {
			continue
		}
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			sup.CloseIfIdle(key)
		}(key)
	}
	wg.Wait()
}

// onBrowserStateChange is the browser's start/stop signal, registered once at
// startup, and called synchronously from inside browser.Session.Start/Stop
// (see notify) — so whatever this does runs on the operator's own "start
// browser"/"stop browser" click, and must stay fast. Which direction it
// fired doesn't change what to do: either way, a warm session's baked-in
// flags are stale and the fix is the same.
func (rt *KarmaxRuntime) onBrowserStateChange(running bool) {
	if rt.harness == nil {
		return
	}
	// A pure flag flip — no I/O — so it costs nothing to do inline. The next
	// turn that calls browserMCPConfig probes again and repopulates it
	// lazily.
	if rt.browserMCPCache != nil {
		rt.browserMCPCache.invalidate()
	}
	// The recycling pass is a different matter: it can close several
	// sessions, and a stubborn process takes up to 3s to give up its own
	// SIGKILL fallback (recycleIdleBrowserSessions parallelises that across
	// sessions, but the pass as a whole is still real work). Running it here
	// would make Start/Stop hang for however long that takes; off to a
	// goroutine is how they return immediately instead.
	go rt.recycleForBrowserStateChange(running)
}

// recycleForBrowserStateChange is onBrowserStateChange's slow part, moved
// off the browser's own Start/Stop call path.
func (rt *KarmaxRuntime) recycleForBrowserStateChange(running bool) {
	rows, err := rt.store.ListHarnessSessions()
	if err != nil {
		return
	}
	kindOf := make(map[string]string, len(rows))
	for _, r := range rows {
		kindOf[r.Key] = r.Kind
	}
	recycleIdleBrowserSessions(rt.harness, kindOf, browserKinds)
	rt.log.Info("harness: browser state changed, recycled idle sessions", zap.Bool("running", running))
}

// browserConfigger is the one method chatTurn and the harness sender need
// from the operator's browser, small enough to fake in tests.
type browserConfigger interface {
	MCPConfigJSON(ctx context.Context) (string, error)
}

// browserMCPConfig returns the --mcp-config value to attach for this kind, or
// "" when there is none: the kind doesn't get a browser, or the browser isn't
// running. browser.ErrNotRunning is the normal case — the browser is usually
// closed — so it is never treated as a failure here.
func browserMCPConfig(ctx context.Context, br browserConfigger, kind string) string {
	if br == nil || !browserKinds[kind] {
		return ""
	}
	cfg, err := br.MCPConfigJSON(ctx)
	if err != nil {
		return ""
	}
	return cfg
}

// browserMCPCache stops browserMCPConfig's network probe from running on
// every chat and agent turn against an already-running session.
// --mcp-config is fixed the moment a session spawns (Session.open only reads
// opt.MCPConfig when it actually starts a process — see extraArgs), so a
// value recomputed on a later, warm turn is thrown away; the loopback GET to
// /json/version behind it, and up to alive's 1.5s timeout when the port file
// is stale, was paid for nothing.
//
// Populated lazily on the first miss, so a cold start — nothing cached yet —
// still gets a correct config before the spawn that needs it. Invalidated,
// not reprobed, by onBrowserStateChange: the browser's start/stop signal
// fires synchronously inside browser.Session.Start/Stop (see
// zzz_scratch_blocking_test.go's own investigation of that call chain), so
// invalidate must stay a pure flag flip — the actual reprobe happens lazily
// on whichever turn asks next.
//
// Implements browserConfigger itself, so it is a drop-in wherever
// browser.Shared(...) used to be handed straight to browserMCPConfig or
// harnessSenderFor.
type browserMCPCache struct {
	br browserConfigger

	// mu guards valid/cfg/err/gen: turns call MCPConfigJSON from whatever
	// goroutine is running that turn, and onBrowserStateChange calls
	// invalidate from the browser's own goroutine — genuinely concurrent,
	// not merely theoretically so.
	mu    sync.Mutex
	valid bool
	cfg   string
	err   error
	// gen counts invalidations. refresh captures it before starting its own
	// probe (up to browser.Shared's ~1.5s alive timeout, run unlocked) and
	// compares it after: if invalidate bumped gen while that probe was in
	// flight, the probe's answer describes a browser state that has already
	// been superseded, and writing it to valid/cfg/err would silently undo
	// the invalidation — pinning the cache to the pre-transition answer
	// until some unrelated later toggle happens to invalidate it again. See
	// refresh's own comment.
	gen uint64
}

func newBrowserMCPCache(br browserConfigger) *browserMCPCache {
	return &browserMCPCache{br: br}
}

// MCPConfigJSON returns the cached answer, probing for the first time (or
// again, after an invalidate) when there isn't one yet.
//
// A nil receiver is handled explicitly, not just guarded against at the call
// site: rt.browserMCPCache boxed into the browserConfigger interface is a
// non-nil interface holding a nil pointer, so browserMCPConfig's own `br ==
// nil` check would not catch it, and the call would reach here.
func (c *browserMCPCache) MCPConfigJSON(ctx context.Context) (string, error) {
	if c == nil {
		return "", browser.ErrNotRunning
	}
	c.mu.Lock()
	if c.valid {
		cfg, err := c.cfg, c.err
		c.mu.Unlock()
		return cfg, err
	}
	c.mu.Unlock()
	return c.refresh(ctx)
}

// refresh does the one real probe and remembers the answer, whichever it
// is: the browser being closed caches exactly as validly as it running —
// that is the normal state, not a miss to keep retrying.
//
// The probe runs unlocked (it is the up-to-1.5s loopback call, not a memory
// operation), so a concurrent invalidate can fire — and complete — while it
// is still in flight. Without the generation check below, that race is
// reproducible deterministically, not just theoretically: start a refresh,
// block it mid-probe, call invalidate, let the probe finish — the write at
// the bottom of this function would set valid back to true holding the
// answer from before whatever invalidate was announcing, and nothing short
// of another, unrelated invalidate would ever look again. Comparing gen
// before writing is what lets this refresh recognise its own answer as
// already stale and discard it instead of caching it.
func (c *browserMCPCache) refresh(ctx context.Context) (string, error) {
	c.mu.Lock()
	gen := c.gen
	c.mu.Unlock()

	cfg, err := c.br.MCPConfigJSON(ctx)

	c.mu.Lock()
	if c.gen == gen {
		c.valid, c.cfg, c.err = true, cfg, err
	}
	// else: invalidated while the probe was in flight. valid is already
	// false from that invalidate; leave it there rather than overwrite it
	// with an answer that predates it. The caller that triggered THIS probe
	// still gets what it asked for below — only the cache write is skipped.
	c.mu.Unlock()
	return cfg, err
}

// invalidate discards the cached answer so the next MCPConfigJSON call
// probes again. Deliberately does no I/O itself: onBrowserStateChange calls
// this synchronously from inside the browser's own Start/Stop, which must
// not block on a network round trip it doesn't need yet.
func (c *browserMCPCache) invalidate() {
	c.mu.Lock()
	c.valid = false
	c.gen++
	c.mu.Unlock()
}

// harnessSenderFor adapts the supervisor to what internal/agent expects,
// keeping the agent package free of this one's types.
type harnessSenderFor struct {
	sup     *harness.Supervisor
	browser browserConfigger
}

func (h harnessSenderFor) Send(ctx context.Context, key, kind, text string) (agent.HarnessTurn, error) {
	turn, err := h.sup.SendWith(ctx, key, kind, text, harness.Options{
		MCPConfig: browserMCPConfig(ctx, h.browser, kind),
	})
	if err != nil {
		var open harness.ErrBreakerOpen
		if asBreakerOpen(err, &open) {
			// Not an error to the caller: the brain falls back on this.
			return agent.HarnessTurn{Available: false, Reason: open.Reason}, nil
		}
		return agent.HarnessTurn{Available: false, Reason: err.Error()}, err
	}
	calls := make([]agent.HarnessToolCall, 0, len(turn.ToolCalls))
	for _, tc := range turn.ToolCalls {
		calls = append(calls, agent.HarnessToolCall{Name: tc.Name, Input: tc.Input})
	}
	return agent.HarnessTurn{Available: true, Text: turn.Text, ToolCalls: calls}, nil
}

// wireHarnessBrains points each agent's thinking at a harness session.
//
// One session per agent, not per turn: the orchestrator's conversation is
// continuous, and a session per turn would pay the cold start and the whole
// per-turn overhead every time — which is the arrangement this package exists
// to avoid.
func (rt *KarmaxRuntime) wireHarnessBrains() {
	if rt.harness == nil {
		return
	}
	sender := harnessSenderFor{sup: rt.harness, browser: rt.browserMCPCache}
	for _, a := range rt.agents.List() {
		// No fallback means a declined turn has nowhere to go, and the agent
		// answers nothing at all. The metered path is worse than the harness;
		// it is not worse than silence.
		fallback := a.MainBrain()
		if fallback == nil {
			rt.log.Error("harness: not routing this agent — it has no API session to fall back to",
				zap.String("agent", a.Def().ID))
			continue
		}
		a.SetHarnessBrain(agent.NewHarnessBrain(sender, "agent:"+a.Def().ID, "agent", fallback))
		rt.log.Info("harness: agent thinking routed to a session",
			zap.String("agent", a.Def().ID))
	}
}

// harnessAnswer runs one prompt in a named long-lived session.
//
// The shape every non-agent caller needs: a reply, or a plain "not available"
// so it can take the metered path. Callers pass a stable key so their work
// continues one conversation rather than starting a process per call, which is
// the difference between this being cheaper than the API and being far worse.
func (rt *KarmaxRuntime) harnessAnswer(ctx context.Context, key, prompt string) (string, bool) {
	if rt == nil || rt.harness == nil {
		return "", false
	}
	turn, err := rt.harness.Send(ctx, key, "summary", prompt)
	if err != nil || strings.TrimSpace(turn.Text) == "" {
		return "", false
	}
	return turn.Text, true
}
