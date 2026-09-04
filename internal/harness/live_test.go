package harness

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// memStore is the supervisor's store, in memory, so a live test needs no db.
type memStore struct {
	mu   sync.Mutex
	rows map[string]*SessionRecord
}

func newMemStore() *memStore { return &memStore{rows: map[string]*SessionRecord{}} }

func (m *memStore) SaveHarnessSession(h SessionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.rows[h.Key]; ok {
		h.Turns, h.CostUSD = old.Turns, old.CostUSD
		h.InputTokens, h.OutputTokens, h.CacheRead = old.InputTokens, old.OutputTokens, old.CacheRead
	}
	cp := h
	m.rows[h.Key] = &cp
	return nil
}
func (m *memStore) RecordHarnessTurn(key string, cost float64, in, out, cr int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[key]
	if !ok {
		return nil
	}
	r.Turns++
	r.CostUSD += cost
	r.InputTokens += in
	r.OutputTokens += out
	r.CacheRead += cr
	r.LastActivityAt = at
	return nil
}
func (m *memStore) SetHarnessState(key, state, e string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[key]; ok {
		r.State, r.LastError, r.LastActivityAt = state, e, at
	}
	return nil
}
func (m *memStore) GetHarnessSession(key string) (*SessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[key]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}
func (m *memStore) ListHarnessSessions(states ...string) ([]SessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SessionRecord
	for _, r := range m.rows {
		if len(states) == 0 {
			out = append(out, *r)
			continue
		}
		for _, s := range states {
			if r.State == s {
				out = append(out, *r)
				break
			}
		}
	}
	return out, nil
}
func (m *memStore) DeleteHarnessSession(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, key)
	return nil
}

type testLog struct{ t *testing.T }

func (l testLog) Info(msg string, kv ...any) { l.t.Logf("info: %s %v", msg, kv) }
func (l testLog) Warn(msg string, kv ...any) { l.t.Logf("warn: %s %v", msg, kv) }

// The claim the whole design rests on: a warm session answers far faster than a
// cold process, and remembers the conversation. Skipped unless KARMAX_LIVE_HARNESS
// is set, because it spends real quota.
func TestLiveWarmSessionIsFastAndRemembers(t *testing.T) {
	if os.Getenv("KARMAX_LIVE_HARNESS") == "" {
		t.Skip("set KARMAX_LIVE_HARNESS=1 to run against the real CLI (spends quota)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}

	st := newMemStore()
	sup := New(Config{
		Binary:      "claude",
		WorkdirRoot: t.TempDir(),
		MaxLive:     2,
		Policies: map[string]Policy{
			"chat": {Model: "sonnet", Idle: time.Minute, TurnTimeout: 90 * time.Second, MaxTurns: 10},
		},
		Env:       os.Environ(),
		Allowlist: map[string]bool{"karmax": true},
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	defer sup.Shutdown()

	ctx := context.Background()

	t0 := time.Now()
	if _, err := sup.Send(ctx, "test:warm", "chat", "Reply with exactly: ONE"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	cold := time.Since(t0)

	t1 := time.Now()
	turn2, err := sup.Send(ctx, "test:warm", "chat", "Reply with exactly: TWO")
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	warm := time.Since(t1)

	turn3, err := sup.Send(ctx, "test:warm", "chat", "What did I ask for first? One word.")
	if err != nil {
		t.Fatalf("third turn: %v", err)
	}

	t.Logf("cold=%v warm=%v", cold.Round(time.Millisecond), warm.Round(time.Millisecond))

	if warm >= cold {
		t.Errorf("warm turn (%v) was not faster than cold (%v) — the design's premise", warm, cold)
	}
	if warm > 6*time.Second {
		t.Errorf("warm turn took %v; the metered path it replaces takes 15-18s", warm)
	}
	if turn3.Text == "" {
		t.Error("third turn returned nothing")
	}
	if turn2.CostUSD == 0 {
		t.Error("no cost reported; the ledger would record nothing")
	}

	rec, _ := st.GetHarnessSession("test:warm")
	if rec == nil || rec.Turns != 3 {
		t.Errorf("store recorded %+v, want 3 turns", rec)
	}
	if rec.CacheRead == 0 {
		t.Error("cache reads not recorded — this is the per-turn overhead the economics turn on")
	}
}

// A session whose process dies must come back with its context, not cold.
func TestLiveSessionSurvivesItsProcessDying(t *testing.T) {
	if os.Getenv("KARMAX_LIVE_HARNESS") == "" {
		t.Skip("set KARMAX_LIVE_HARNESS=1 to run against the real CLI (spends quota)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}

	st := newMemStore()
	sup := New(Config{
		Binary: "claude", WorkdirRoot: t.TempDir(), MaxLive: 2,
		Policies: map[string]Policy{"chat": {Model: "sonnet", Idle: time.Minute, TurnTimeout: 90 * time.Second}},
		Env:      os.Environ(),
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	defer sup.Shutdown()

	ctx := context.Background()
	if _, err := sup.Send(ctx, "test:resume", "chat", "Remember the codeword PELICAN. Reply: stored"); err != nil {
		t.Fatalf("first turn: %v", err)
	}

	// Kill the process the way a crash would.
	sup.mu.Lock()
	sess := sup.live["test:resume"]
	sup.mu.Unlock()
	if sess == nil {
		t.Fatal("no live session to kill")
	}
	_ = sess.cmd.Process.Kill()
	time.Sleep(500 * time.Millisecond)

	turn, err := sup.Send(ctx, "test:resume", "chat", "What was the codeword? One word.")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !contains(turn.Text, "PELICAN") {
		t.Errorf("resumed session lost its context: %q", turn.Text)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
