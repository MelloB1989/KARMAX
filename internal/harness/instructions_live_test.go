package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The capability the workflow tier depends on: a session must actually read the
// standing brief its workflow wrote beside it.
//
// Verified against the real CLI, because "CLAUDE.md is picked up" is a claim
// about someone else's program. A shared file at the root and a particular one
// in the session's own directory must BOTH apply — that is what lets KARMAX
// state what is true for every session while a workflow states only its own.
func TestLiveSessionReadsTheBriefItWasGiven(t *testing.T) {
	if os.Getenv("KARMAX_LIVE_HARNESS") == "" {
		t.Skip("set KARMAX_LIVE_HARNESS=1 to run against the real CLI (spends quota)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}

	root := t.TempDir()
	// What KARMAX writes once, for every session.
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"),
		[]byte("# Shared\nThe shared marker is KERNEL.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := newMemStore()
	sup := New(Config{
		Binary: "claude", WorkdirRoot: root, MaxLive: 2,
		Policies: map[string]Policy{"chat": {Model: "sonnet", Idle: time.Minute, TurnTimeout: 90 * time.Second}},
		Env:      os.Environ(),
	}, st, NewBreaker(0.99, nil), testLog{t}, nil)
	defer sup.Shutdown()

	// What a workflow writes for its own conversation.
	workdir := filepath.Join(root, "chats", "demo")
	turn, err := sup.SendWith(context.Background(), "chat:demo", "chat",
		"Reply with two words: the shared marker, then the chat marker.",
		Options{
			Workdir:      workdir,
			Instructions: "# This chat\nThe chat marker is WORKFLOW.\n",
		})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workdir, "CLAUDE.md")); err != nil {
		t.Fatalf("the workflow's brief was never written: %v", err)
	}
	if !strings.Contains(turn.Text, "WORKFLOW") {
		t.Errorf("the session did not read its own brief: %q", turn.Text)
	}
	if !strings.Contains(turn.Text, "KERNEL") {
		t.Errorf("the shared brief did not reach the session: %q", turn.Text)
	}
}
