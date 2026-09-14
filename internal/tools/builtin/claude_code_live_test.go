package builtin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// TestLiveClaudeCodeToolResumesASessionKeyAcrossTurns drives the FIXED
// ClaudeCodeTool twice, with the exact same "lyzn:<id>" session KEY the
// LYZN tasks recipe uses, against the real `claude` CLI — not the fake
// stub the rest of this package's tests use. It proves the whole scheme
// end to end: turn 1 mints a real uuid via --session-id and persists the
// key -> uuid mapping only after the CLI actually created that session;
// turn 2 resolves the same key back to that uuid and --resume's it, and
// the reply shows it remembers turn 1.
//
// Skipped unless KARMAX_LIVE_CLAUDE=1, because it spends real quota — see
// internal/harness/live_test.go for the same convention. harnessEnv()
// (internal/tools/builtin/harness_env.go) already excludes ANTHROPIC_API_KEY
// from what a spawned claude/codex subprocess inherits, by design, so this
// runs on the operator's Claude subscription regardless of what is set in
// the ambient environment.
func TestLiveClaudeCodeToolResumesASessionKeyAcrossTurns(t *testing.T) {
	if os.Getenv("KARMAX_LIVE_CLAUDE") == "" {
		t.Skip("set KARMAX_LIVE_CLAUDE=1 to run against the real claude CLI (spends quota)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}

	root := t.TempDir()
	t.Setenv("KARMAX_WORKDIR", root)
	hostpaths.ResetWorkDirForTest()
	t.Cleanup(hostpaths.ResetWorkDirForTest)

	s, err := store.New(filepath.Join(t.TempDir(), "karmax.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	tool := &ClaudeCodeTool{Store: s, AgentID: "agent-live"}
	workingDir := "live-session-key-test"
	key := "lyzn:live-t-1"

	res1, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "Reply with exactly: pineapple", "session_id": key, "working_dir": workingDir,
	})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if res1.IsError {
		t.Fatalf("turn 1 failed: %+v", res1)
	}
	t.Logf("turn 1 output: %+v", res1.Output)

	mappedAfterTurn1, err := s.GetSessionKey(key)
	if err != nil {
		t.Fatalf("GetSessionKey after turn 1: %v", err)
	}
	if mappedAfterTurn1 == "" {
		t.Fatalf("no session-key mapping was persisted after a successful turn 1")
	}
	t.Logf("mapped uuid after turn 1: %s", mappedAfterTurn1)

	res2, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "What word did you just reply with? One word only, lowercase.", "session_id": key, "working_dir": workingDir,
	})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if res2.IsError {
		t.Fatalf("turn 2 failed: %+v", res2)
	}
	t.Logf("turn 2 output: %+v", res2.Output)

	out2, _ := res2.Output.(map[string]any)
	text, _ := out2["output"].(string)
	if !strings.Contains(strings.ToLower(text), "pineapple") {
		t.Fatalf("turn 2 shows no memory of turn 1 (expected \"pineapple\" in the reply): %q", text)
	}

	mappedAfterTurn2, err := s.GetSessionKey(key)
	if err != nil {
		t.Fatalf("GetSessionKey after turn 2: %v", err)
	}
	if mappedAfterTurn2 != mappedAfterTurn1 {
		t.Fatalf("mapping changed across turns: %q -> %q, want the same uuid resumed both times", mappedAfterTurn1, mappedAfterTurn2)
	}
}
