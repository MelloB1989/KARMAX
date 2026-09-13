package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// stubbornFakeClaude writes a script that answers every turn immediately but
// ignores SIGINT/SIGTERM (bash's default disposition applies to everything
// else, but nothing here traps these two), forcing Session.Close() through
// its full ~3s SIGKILL fallback — the slow case that used to sit on
// onBrowserStateChange's critical path when recycling ran inline.
func stubbornFakeClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stubborn.sh")
	body := "#!/bin/bash\n" +
		"trap '' INT TERM\n" +
		"while IFS= read -r line; do\n" +
		"  echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"ok\",\"total_cost_usd\":0.001,\"num_turns\":1,\"duration_ms\":10,\"usage\":{}}'\n" +
		"done\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// runtimeForRecycleTest builds a KarmaxRuntime over a real store and a real
// harness.Supervisor running the stubborn script, with one chat session
// already warm and idle — the state onBrowserStateChange finds when the
// operator's browser toggle fires.
func runtimeForRecycleTest(t *testing.T) *KarmaxRuntime {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "t.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	sup := harness.New(harness.Config{
		Binary: stubbornFakeClaude(t), WorkdirRoot: t.TempDir(), MaxLive: 8,
		Policies: map[string]harness.Policy{"chat": {Idle: time.Minute, TurnTimeout: 5 * time.Second}},
		Env:      os.Environ(),
	}, harnessStore{db}, harness.NewBreaker(0.95, nil), harnessLog{zap.NewNop()}, nil)
	t.Cleanup(sup.Shutdown)

	if _, err := sup.Send(context.Background(), "chat:1", "chat", "hi"); err != nil {
		t.Fatalf("warm-up turn: %v", err)
	}

	return &KarmaxRuntime{store: db, log: zap.NewNop(), harness: sup}
}

// onBrowserStateChange runs synchronously inside browser.Session.Start/Stop
// (see notify) — so whatever it does blocks the operator's own "start
// browser"/"stop browser" click. A stubborn session can take ~3s to actually
// close; this call must return long before that, not after.
func TestOnBrowserStateChangeReturnsPromptlyWithAStubbornSession(t *testing.T) {
	rt := runtimeForRecycleTest(t)

	start := time.Now()
	rt.onBrowserStateChange(true)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("onBrowserStateChange took %v to return; a stubborn session's ~3s "+
			"SIGKILL fallback must not be on this call's critical path", elapsed)
	}

	// The recycling it dispatched must still actually happen — this is
	// "moved off the critical path", not "dropped".
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(rt.harness.Live()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session was never recycled: still live after 5s")
}
