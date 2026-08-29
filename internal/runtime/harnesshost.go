package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
			rt.log.Warn("harness: standing down", zap.String("reason", reason))
			builtin.PushAppNotification(rt.store, "", "alert",
				"Harness paused", reason+" — KARMAX is falling back to the metered API path.")
			return
		}
		rt.log.Info("harness: available again", zap.String("was", reason))
		builtin.PushAppNotification(rt.store, "", "info",
			"Harness resumed", "The rate-limit window rolled; KARMAX is using the harness again.")
	})

	sup := harness.New(harness.Config{
		Binary:      hc.Binary,
		WorkdirRoot: root,
		MaxLive:     hc.MaxLive,
		Policies:    policies,
		Env:         harnessEnviron(),
		Allowlist:   allow,
	}, harnessStore{rt.store}, breaker, harnessLog{rt.log}, rt.auditHarnessTool)

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
