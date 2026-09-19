package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/chatlog"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// fakeClaudeForHarnessWith stands a fake `claude` CLI in on PATH and
// sandboxes HOME, the same way internal/tools/builtin's own fake harness
// does (see that package's claude_code_session_key_test.go for the full
// rationale) — duplicated here rather than shared because Go test helpers
// don't cross package boundaries, and this one only needs the two shapes
// HarnessWith's own tests exercise: an unconditional failure, and a normal
// success that leaves a transcript behind.
func fakeClaudeForHarnessWith(t *testing.T) (home string) {
	t.Helper()
	binDir := t.TempDir()
	home = t.TempDir()

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
	t.Setenv("KARMAX_HARNESS_ENV_PASSTHROUGH", "KARMAX_TEST_CLAUDE_WORKDIR,KARMAX_TEST_CLAUDE_FAIL")
	return home
}

// newHarnessWithTestKit builds just enough of a KarmaxRuntime/loopKit for
// HarnessWith to run its real path — ClaudeCodeTool.Execute and back —
// without standing up the rest of the daemon.
func newHarnessWithTestKit(t *testing.T) *loopKit {
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

	rt := &KarmaxRuntime{store: s, log: zap.NewNop(), cfg: &config.KarmaxConfig{}}
	return &loopKit{rt: rt, agentID: "agent-1"}
}

// TestHarnessWithSurfacesAFailedCLIRunAsAnError is the laundering fix's own
// regression test at the loopKit boundary the recipe engine actually calls
// through (recipes.runStep's VerbHarness case → k.HarnessWith). Before the
// fix, ClaudeCodeTool.run always returned tools.SuccessResult even when the
// CLI exited non-zero, so this returned (a stderr string, nil) — a "result"
// the LYZN tasks recipe would have POSTed to /result as the operator's
// answer.
func TestHarnessWithSurfacesAFailedCLIRunAsAnError(t *testing.T) {
	fakeClaudeForHarnessWith(t)
	k := newHarnessWithTestKit(t)
	t.Setenv("KARMAX_TEST_CLAUDE_FAIL", "1")

	_, err := k.HarnessWith(context.Background(), loopkit.HarnessSpec{
		Prompt: "do the thing", WorkingDir: "task-err", Ephemeral: true,
	})
	if err == nil {
		t.Fatal("HarnessWith returned no error for a failed CLI run; the recipe engine would treat this as a real reply")
	}
}

// TestHarnessWithSucceedsOnAWorkingCLIRun is the control: the fix must not
// turn a genuinely successful run into an error too.
func TestHarnessWithSucceedsOnAWorkingCLIRun(t *testing.T) {
	fakeClaudeForHarnessWith(t)
	k := newHarnessWithTestKit(t)

	res, err := k.HarnessWith(context.Background(), loopkit.HarnessSpec{
		Prompt: "do the thing", WorkingDir: "task-ok", Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("HarnessWith: %v", err)
	}
	if res.Output == "" {
		t.Fatalf("expected non-empty output from a successful run, got %+v", res)
	}
}

// TestPruneStaleLyznSessionsRemovesTranscriptAndSessionKeyMapping is the
// six-hour sweep's own coverage of the key scheme: it must resolve the
// "lyzn:<task id>" key it selects by to the real uuid before it can delete
// anything, and it must also delete the mapping row itself — otherwise a
// swept task's next run (if the operator later resurrects it) would find a
// mapping pointing at a transcript that is already gone.
func TestPruneStaleLyznSessionsRemovesTranscriptAndSessionKeyMapping(t *testing.T) {
	home := fakeClaudeForHarnessWith(t)
	root := t.TempDir()
	t.Setenv("KARMAX_WORKDIR", root)
	hostpaths.ResetWorkDirForTest()
	t.Cleanup(hostpaths.ResetWorkDirForTest)

	s, err := store.New(filepath.Join(t.TempDir(), "test.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	workingDir := filepath.Join("lyzn-tasks", "task-9")
	resolved := hostpaths.Resolve(workingDir)
	t.Setenv("KARMAX_TEST_CLAUDE_WORKDIR", resolved)

	tool := &builtin.ClaudeCodeTool{Store: s}
	if _, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "do the thing", "session_id": "lyzn:task-9", "working_dir": workingDir,
	}); err != nil {
		t.Fatalf("seed turn: %v", err)
	}

	mappedUUID, err := s.GetSessionKey("lyzn:task-9")
	if err != nil || mappedUUID == "" {
		t.Fatalf("expected a mapping after the seed turn, got %q, err=%v", mappedUUID, err)
	}
	transcript := filepath.Join(home, ".claude", "projects", chatlog.Slug(resolved), mappedUUID+".jsonl")
	if _, err := os.Stat(transcript); err != nil {
		t.Fatalf("expected a transcript at %s: %v", transcript, err)
	}

	// The seed turn just ran, so its coding_sessions row is fresh; back it
	// off past the sweep's cutoff by session_id (its row id is internal to
	// ClaudeCodeTool.run and not returned to the caller).
	if _, err := s.DB().Exec(`UPDATE coding_sessions SET updated_at = ? WHERE session_id = ?`,
		time.Now().Add(-8*24*time.Hour).UTC(), "lyzn:task-9"); err != nil {
		t.Fatalf("backdate seeded row: %v", err)
	}

	rt := &KarmaxRuntime{store: s, log: zap.NewNop()}
	rt.pruneStaleLyznSessions(lyznSessionStaleCutoff(time.Now()))

	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatalf("transcript still exists after the sweep: %v", err)
	}
	if left, err := s.GetSessionKey("lyzn:task-9"); err != nil || left != "" {
		t.Fatalf("mapping still exists after the sweep: %q, err=%v", left, err)
	}
}
