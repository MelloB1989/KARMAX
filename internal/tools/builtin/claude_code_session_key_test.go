package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/chatlog"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The bug this covers: the LYZN tasks recipe passes session_id: "lyzn:<task
// id>" — a caller-chosen KEY, not a Claude Code session id — because the
// recipe has no way to mint or remember a uuid of its own. The CLI accepts
// only a real UUID for --session-id/--resume, so every first turn used to
// try `--resume lyzn:<id>` against a session that had never existed, fail,
// and (before the laundering fix) still report success. The fix: a
// non-UUID session_id is treated as a stable key, resolved through the
// store's key->uuid mapping (see internal/store/coding_store.go).
//
// fakeClaudeHarness below stands in for the real CLI closely enough to
// drive ClaudeCodeTool.run's actual exec.Command path — see its own comment
// for exactly what it simulates and why.

// fakeClaudeHarness installs a fake `claude` binary on PATH for one test and
// sandboxes HOME to a fresh temp directory, so the transcripts this writes
// (and ClaudeCodeTool's own chatlog lookups) never touch a developer's real
// ~/.claude. Returns the sandboxed HOME and the path to a log file the fake
// binary appends "<mode> <session-id>" to on every invocation — "new" for
// --session-id, "resume" for --resume — so a test can assert exactly which
// flag ClaudeCodeTool.run used without inferring it indirectly.
//
// Behaviour the fake reproduces, closely enough to exercise the real bug and
// the real fix without guessing at CLI internals from memory:
//   - `--session-id <uuid>`: always succeeds, writes a transcript for <uuid>.
//   - `--resume <uuid>`: succeeds ONLY if a transcript for <uuid> already
//     exists (a session actually created by an earlier `--session-id` call
//     in this same test) — otherwise it fails with the CLI's own "No
//     conversation found with session ID: <uuid>" text and exit 1. This
//     reproduces a stale mapping by construction (resume a uuid nothing
//     created) rather than a separate simulated switch.
//   - KARMAX_TEST_CLAUDE_FAIL, when set, fails unconditionally with a
//     generic message and writes no transcript — for a turn that fails for
//     a reason that has nothing to do with session identity.
func fakeClaudeHarness(t *testing.T) (home, logPath string) {
	t.Helper()
	binDir := t.TempDir()
	home = t.TempDir()
	logPath = filepath.Join(t.TempDir(), "claude-invocations.log")

	script := `#!/bin/bash
workdir="${KARMAX_TEST_CLAUDE_WORKDIR:-$(pwd)}"
mode=""
sid=""
prev=""
for a in "$@"; do
  case "$prev" in
    --session-id) sid="$a"; mode="new" ;;
    --resume) sid="$a"; mode="resume" ;;
  esac
  prev="$a"
done

if [ -n "${KARMAX_TEST_CLAUDE_LOG:-}" ]; then
  echo "$mode $sid" >> "$KARMAX_TEST_CLAUDE_LOG"
fi

if [ -n "${KARMAX_TEST_CLAUDE_FAIL:-}" ]; then
  echo "simulated failure for session $sid"
  exit 1
fi

slug=$(printf '%s' "$workdir" | tr '/._' '---')
dir="$HOME/.claude/projects/$slug"
mkdir -p "$dir"
transcript="$dir/$sid.jsonl"

if [ "$mode" = "resume" ] && [ ! -f "$transcript" ]; then
  echo "No conversation found with session ID: $sid"
  exit 1
fi

echo '{"turn":true}' >> "$transcript"
echo "ok: turn for session $sid"
exit 0
`
	path := filepath.Join(binDir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", home)
	t.Setenv("KARMAX_TEST_CLAUDE_LOG", logPath)
	t.Setenv("KARMAX_HARNESS_ENV_PASSTHROUGH",
		"KARMAX_TEST_CLAUDE_WORKDIR,KARMAX_TEST_CLAUDE_FAIL,KARMAX_TEST_CLAUDE_LOG")
	return home, logPath
}

// invocations reads back one "<mode> <session-id>" line per fake-claude call.
func invocations(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read invocation log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// newSessionKeyTestTool returns a ClaudeCodeTool backed by a fresh in-memory
// store and a shared-root stand-in, plus the (relative) working_dir every
// test in this file uses.
func newSessionKeyTestTool(t *testing.T) (*ClaudeCodeTool, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("KARMAX_WORKDIR", root)
	hostpaths.ResetWorkDirForTest()
	t.Cleanup(hostpaths.ResetWorkDirForTest)

	s, err := store.New(filepath.Join(t.TempDir(), "karmax.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	return &ClaudeCodeTool{Store: s, AgentID: "agent-1"}, filepath.Join("lyzn-tasks", "task-1")
}

func TestFirstTurnWithNewKeyUsesSessionIDFlagAndPersistsMapping(t *testing.T) {
	_, logPath := fakeClaudeHarness(t)
	tool, workingDir := newSessionKeyTestTool(t)
	t.Setenv("KARMAX_TEST_CLAUDE_WORKDIR", hostpaths.Resolve(workingDir))

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "do the thing", "session_id": "lyzn:task-1",
		"working_dir": workingDir, "ephemeral": false,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("Execute returned an error result: %+v", res)
	}

	lines := invocations(t, logPath)
	if len(lines) != 1 {
		t.Fatalf("got %d claude invocations, want 1: %v", len(lines), lines)
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 || fields[0] != "new" {
		t.Fatalf("first turn invocation = %q, want mode \"new\" (--session-id) with a session id, not --resume", lines[0])
	}
	mintedUUID := fields[1]
	if _, err := uuid.Parse(mintedUUID); err != nil {
		t.Fatalf("minted session id %q is not a valid uuid: %v", mintedUUID, err)
	}

	mapped, err := tool.Store.GetSessionKey("lyzn:task-1")
	if err != nil {
		t.Fatalf("GetSessionKey: %v", err)
	}
	if mapped != mintedUUID {
		t.Fatalf("persisted mapping = %q, want the minted uuid %q", mapped, mintedUUID)
	}
}

func TestSecondTurnWithSameKeyResumesTheSameUUID(t *testing.T) {
	_, logPath := fakeClaudeHarness(t)
	tool, workingDir := newSessionKeyTestTool(t)
	t.Setenv("KARMAX_TEST_CLAUDE_WORKDIR", hostpaths.Resolve(workingDir))

	input := map[string]any{"prompt": "do the thing", "session_id": "lyzn:task-1", "working_dir": workingDir}
	if _, err := tool.Execute(context.Background(), input); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := tool.Execute(context.Background(), input); err != nil {
		t.Fatalf("second turn: %v", err)
	}

	lines := invocations(t, logPath)
	if len(lines) != 2 {
		t.Fatalf("got %d invocations, want 2: %v", len(lines), lines)
	}
	first := strings.Fields(lines[0])
	second := strings.Fields(lines[1])
	if first[0] != "new" {
		t.Fatalf("first turn mode = %q, want \"new\"", first[0])
	}
	if second[0] != "resume" {
		t.Fatalf("second turn mode = %q, want \"resume\", not a fresh --session-id", second[0])
	}
	if second[1] != first[1] {
		t.Fatalf("second turn resumed uuid %q, want the first turn's uuid %q", second[1], first[1])
	}
}

func TestFirstTurnFailureDoesNotPersistAMapping(t *testing.T) {
	fakeClaudeHarness(t)
	tool, workingDir := newSessionKeyTestTool(t)
	t.Setenv("KARMAX_TEST_CLAUDE_WORKDIR", hostpaths.Resolve(workingDir))
	t.Setenv("KARMAX_TEST_CLAUDE_FAIL", "1")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "do the thing", "session_id": "lyzn:task-fail", "working_dir": workingDir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a failed CLI run must surface as an error result, got %+v", res)
	}
	if strings.TrimSpace(res.Error) == "" {
		t.Fatalf("error result carries no error text: %+v", res)
	}

	mapped, err := tool.Store.GetSessionKey("lyzn:task-fail")
	if err != nil {
		t.Fatalf("GetSessionKey: %v", err)
	}
	if mapped != "" {
		t.Fatalf("got mapping %q persisted for a failed first turn, want none", mapped)
	}
}

func TestStaleMappingRecoversWithAFreshSessionWithoutFailingTheTask(t *testing.T) {
	_, logPath := fakeClaudeHarness(t)
	tool, workingDir := newSessionKeyTestTool(t)
	t.Setenv("KARMAX_TEST_CLAUDE_WORKDIR", hostpaths.Resolve(workingDir))

	staleUUID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	if err := tool.Store.SaveSessionKey("lyzn:task-stale", staleUUID, "claude_code"); err != nil {
		t.Fatalf("seed stale mapping: %v", err)
	}
	// Deliberately no transcript on disk for staleUUID: the fake CLI's
	// --resume will report "No conversation found", exactly as a deleted
	// transcript or a migrated machine would in reality.

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "do the thing", "session_id": "lyzn:task-stale", "working_dir": workingDir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("a stale mapping must not fail the task, got error result: %+v", res)
	}

	lines := invocations(t, logPath)
	if len(lines) != 2 {
		t.Fatalf("got %d invocations, want 2 (a failed resume, then a fresh session): %v", len(lines), lines)
	}
	first := strings.Fields(lines[0])
	second := strings.Fields(lines[1])
	if first[0] != "resume" || first[1] != staleUUID {
		t.Fatalf("first invocation = %q, want a resume of the stale uuid %q", lines[0], staleUUID)
	}
	if second[0] != "new" {
		t.Fatalf("recovery invocation = %q, want a fresh --session-id turn", lines[1])
	}
	if second[1] == staleUUID {
		t.Fatalf("recovery reused the stale uuid instead of minting a fresh one")
	}

	mapped, err := tool.Store.GetSessionKey("lyzn:task-stale")
	if err != nil {
		t.Fatalf("GetSessionKey: %v", err)
	}
	if mapped != second[1] {
		t.Fatalf("mapping after recovery = %q, want the fresh uuid %q", mapped, second[1])
	}
}

func TestValidUUIDSessionIDBehavesAsBeforeNoMapping(t *testing.T) {
	_, logPath := fakeClaudeHarness(t)
	tool, workingDir := newSessionKeyTestTool(t)
	t.Setenv("KARMAX_TEST_CLAUDE_WORKDIR", hostpaths.Resolve(workingDir))

	explicit := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	if _, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "do the thing", "session_id": explicit, "working_dir": workingDir,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	lines := invocations(t, logPath)
	if len(lines) != 1 {
		t.Fatalf("got %d invocations, want 1: %v", len(lines), lines)
	}
	fields := strings.Fields(lines[0])
	if fields[0] != "resume" || fields[1] != explicit {
		t.Fatalf("invocation = %q, want --resume of the exact given uuid (today's unchanged behaviour for a valid-UUID session_id)", lines[0])
	}

	if mapped, err := tool.Store.GetSessionKey(explicit); err != nil || mapped != "" {
		t.Fatalf("a valid-UUID session_id must never create a session-key mapping; got %q, err=%v", mapped, err)
	}
}

func TestCleanupResolvesSessionKeyDeletesTranscriptAndMapping(t *testing.T) {
	home, _ := fakeClaudeHarness(t)
	tool, workingDir := newSessionKeyTestTool(t)
	resolved := hostpaths.Resolve(workingDir)
	t.Setenv("KARMAX_TEST_CLAUDE_WORKDIR", resolved)

	if _, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "do the thing", "session_id": "lyzn:task-cleanup", "working_dir": workingDir,
	}); err != nil {
		t.Fatalf("seed turn: %v", err)
	}

	mappedUUID, err := tool.Store.GetSessionKey("lyzn:task-cleanup")
	if err != nil || mappedUUID == "" {
		t.Fatalf("expected a mapping after a successful turn, got %q, err=%v", mappedUUID, err)
	}
	transcript := filepath.Join(home, ".claude", "projects", chatlog.Slug(resolved), mappedUUID+".jsonl")
	if _, err := os.Stat(transcript); err != nil {
		t.Fatalf("expected a transcript at %s: %v", transcript, err)
	}

	if err := tool.Cleanup(workingDir, "lyzn:task-cleanup"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatalf("transcript still exists after Cleanup: %v", err)
	}
	if left, err := tool.Store.GetSessionKey("lyzn:task-cleanup"); err != nil || left != "" {
		t.Fatalf("mapping still exists after Cleanup: %q, err=%v", left, err)
	}
}
