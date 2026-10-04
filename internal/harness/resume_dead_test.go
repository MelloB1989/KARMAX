package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeArgRecordingClaude is the fake CLI from closeifidle_test.go that also
// writes the arguments it was started with, so a test can see whether the
// supervisor resumed a conversation or began a new one.
func writeArgRecordingClaude(t *testing.T) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	bin = filepath.Join(dir, "fakeclaude.sh")
	body := "#!/bin/bash\n" +
		"echo \"$@\" > " + argsFile + "\n" +
		"while IFS= read -r line; do\n" +
		"  echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"ok\",\"total_cost_usd\":0.001,\"num_turns\":1,\"duration_ms\":10,\"usage\":{}}'\n" +
		"done\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

func supervisorWith(t *testing.T, bin string, st *memStore) *Supervisor {
	t.Helper()
	sup := New(Config{
		Binary: bin, WorkdirRoot: t.TempDir(), MaxLive: 8,
		Policies: map[string]Policy{"chat": {Idle: time.Minute, TurnTimeout: 5 * time.Second}},
		Env:      os.Environ(),
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	return sup
}

// The api/main/nexus failure: a session whose first turn never completed left
// a record naming a conversation Claude Code never wrote. Resuming it fails at
// once, on every attempt. It must start over instead.
func TestARecordThatNeverCompletedATurnStartsFresh(t *testing.T) {
	bin, argsFile := writeArgRecordingClaude(t)
	st := newMemStore()
	const dead = "c5d577fe-e07d-4f0d-bd37-e357914d9f42"
	now := time.Now()
	_ = st.SaveHarnessSession(SessionRecord{Key: "api/main/nexus", HarnessSessionID: dead, Kind: "chat",
		State: "dead", StartedAt: now, LastActivityAt: now, Turns: 0})

	sup := supervisorWith(t, bin, st)
	if _, err := sup.Send(context.Background(), "api/main/nexus", "chat", "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "--resume") {
		t.Fatalf("a session that never completed a turn was resumed: %s", args)
	}
	if strings.Contains(args, dead) {
		t.Fatalf("the dead conversation id was reused: %s", args)
	}
	if !strings.Contains(args, "--session-id") {
		t.Fatalf("no new session id was given: %s", args)
	}
}

// A session with real history is still resumed: that is what brings an agent's
// context back after a restart, and it must not regress.
func TestASessionWithHistoryIsResumed(t *testing.T) {
	bin, argsFile := writeArgRecordingClaude(t)
	st := newMemStore()
	const live = "7d038337-e2a5-4008-a8f5-1eca783d6d5d"
	now := time.Now()
	_ = st.SaveHarnessSession(SessionRecord{Key: "agent:nexus", HarnessSessionID: live, Kind: "chat",
		State: "closed", StartedAt: now, LastActivityAt: now, Turns: 503})

	sup := supervisorWith(t, bin, st)
	if _, err := sup.Send(context.Background(), "agent:nexus", "chat", "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if args := readArgs(t, argsFile); !strings.Contains(args, "--resume "+live) {
		t.Fatalf("a session with 503 turns was not resumed: %s", args)
	}
}

func readArgs(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake CLI never recorded its arguments")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Past its turn limit a session starts over instead of resuming a transcript it keeps getting closed for.
func TestASessionPastItsTurnLimitStartsFresh(t *testing.T) {
	bin, argsFile := writeArgRecordingClaude(t)
	st := newMemStore()
	const full = "7d038337-e2a5-4008-a8f5-1eca783d6d5d"
	now := time.Now()
	_ = st.SaveHarnessSession(SessionRecord{Key: "agent:nexus", HarnessSessionID: full, Kind: "chat",
		State: "closed", StartedAt: now, LastActivityAt: now, Turns: 512})
	sup := New(Config{
		Binary: bin, WorkdirRoot: t.TempDir(), MaxLive: 8,
		Policies: map[string]Policy{"chat": {Idle: time.Minute, TurnTimeout: 5 * time.Second, MaxTurns: 500}},
		Env:      os.Environ(),
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)

	if _, err := sup.Send(context.Background(), "agent:nexus", "chat", "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if args := readArgs(t, argsFile); strings.Contains(args, full) {
		t.Fatalf("a session past its turn limit was resumed: %s", args)
	}
	if rec, _ := st.GetHarnessSession("agent:nexus"); rec == nil || rec.Turns != 1 {
		t.Fatalf("the fresh session should count from zero, got %+v", rec)
	}
}
