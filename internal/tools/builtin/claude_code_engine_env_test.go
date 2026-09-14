package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// The env-injection half of least-privilege engine access: when
// ClaudeCodeTool.EngineAPIURL/EngineBrowserToken are set, runCLIOnce must
// hand the spawned harness KARMAX_API_URL and KARMAX_API_TOKEN so `karmax
// browser ...` run from inside it can reach the engine — but the value it
// gets for KARMAX_API_TOKEN must be exactly EngineBrowserToken (a token
// scoped server-side to the browser tool only, see internal/api), never
// anything else. When the fields are left empty (today's every caller,
// pre-existing behaviour), neither variable should appear at all.

// fakeClaudeThatDumpsEnv installs a fake `claude` binary that, instead of
// doing anything with the CLI flags, writes what it actually saw for
// KARMAX_API_URL and KARMAX_API_TOKEN to envLogPath — one "KEY=value" line
// each, or "KEY=<unset>" if the child process's env held nothing for that
// key at all (distinguishing "not set" from "set to empty", though this
// suite never sets either to empty). It also writes a minimal transcript so
// ClaudeCodeTool.run's own bookkeeping succeeds, the same way
// fakeClaudeHarness (claude_code_session_key_test.go) does, so a real,
// complete Execute() call can be driven end to end rather than just
// exec'ing the binary directly.
func fakeClaudeThatDumpsEnv(t *testing.T) (home, envLogPath string) {
	t.Helper()
	binDir := t.TempDir()
	home = t.TempDir()
	envLogPath = filepath.Join(t.TempDir(), "claude-env.log")

	script := `#!/bin/bash
workdir="${KARMAX_TEST_CLAUDE_WORKDIR:-$(pwd)}"
sid=""
prev=""
for a in "$@"; do
  case "$prev" in
    --session-id) sid="$a" ;;
    --resume) sid="$a" ;;
  esac
  prev="$a"
done

if [ -n "${KARMAX_TEST_ENV_LOG:-}" ]; then
  {
    echo "KARMAX_API_URL=${KARMAX_API_URL:-<unset>}"
    echo "KARMAX_API_TOKEN=${KARMAX_API_TOKEN:-<unset>}"
  } > "$KARMAX_TEST_ENV_LOG"
fi

slug=$(printf '%s' "$workdir" | tr '/._' '---')
dir="$HOME/.claude/projects/$slug"
mkdir -p "$dir"
echo '{"turn":true}' >> "$dir/$sid.jsonl"
echo "ok: turn for session $sid"
exit 0
`
	path := filepath.Join(binDir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", home)
	t.Setenv("KARMAX_TEST_ENV_LOG", envLogPath)
	// Deliberately NOT including KARMAX_API_URL/KARMAX_API_TOKEN here: those
	// two must reach the child ONLY via ClaudeCodeTool's own EngineAPIURL/
	// EngineBrowserToken -> runCLIOnce path, not by riding through the
	// ordinary harnessEnv() passthrough allowlist.
	t.Setenv("KARMAX_HARNESS_ENV_PASSTHROUGH", "KARMAX_TEST_CLAUDE_WORKDIR,KARMAX_TEST_ENV_LOG")
	return home, envLogPath
}

// envLogLines reads back the fake claude's env dump as a key->value map.
func envLogLines(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		if i := strings.IndexByte(line, '='); i >= 0 {
			out[line[:i]] = line[i+1:]
		}
	}
	return out
}

// newEngineEnvTestTool returns a ClaudeCodeTool backed by a fresh in-memory
// store and a shared-root stand-in, mirroring newSessionKeyTestTool in
// claude_code_session_key_test.go.
func newEngineEnvTestTool(t *testing.T) (*ClaudeCodeTool, string) {
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

func TestEngineFieldsSetInjectScopedURLAndToken(t *testing.T) {
	_, envLogPath := fakeClaudeThatDumpsEnv(t)
	tool, workingDir := newEngineEnvTestTool(t)
	t.Setenv("KARMAX_TEST_CLAUDE_WORKDIR", hostpaths.Resolve(workingDir))

	tool.EngineAPIURL = "http://localhost:9091"
	tool.EngineBrowserToken = "browser-scoped-token-abc"

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "do the thing", "session_id": "lyzn:task-1", "working_dir": workingDir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("Execute returned an error result: %+v", res)
	}

	env := envLogLines(t, envLogPath)
	if env["KARMAX_API_URL"] != "http://localhost:9091" {
		t.Fatalf("KARMAX_API_URL = %q, want the engine base URL", env["KARMAX_API_URL"])
	}
	if env["KARMAX_API_TOKEN"] != "browser-scoped-token-abc" {
		t.Fatalf("KARMAX_API_TOKEN = %q, want the browser-scoped token", env["KARMAX_API_TOKEN"])
	}
}

func TestEngineFieldsEmptyInjectNeitherVar(t *testing.T) {
	_, envLogPath := fakeClaudeThatDumpsEnv(t)
	tool, workingDir := newEngineEnvTestTool(t)
	t.Setenv("KARMAX_TEST_CLAUDE_WORKDIR", hostpaths.Resolve(workingDir))
	// EngineAPIURL/EngineBrowserToken left at their zero value: every
	// existing caller of ClaudeCodeTool before this change, and any caller
	// that has no engine API to offer (API server disabled).

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "do the thing", "session_id": "lyzn:task-1", "working_dir": workingDir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("Execute returned an error result: %+v", res)
	}

	env := envLogLines(t, envLogPath)
	if env["KARMAX_API_URL"] != "<unset>" {
		t.Fatalf("KARMAX_API_URL = %q, want unset (unchanged behaviour)", env["KARMAX_API_URL"])
	}
	if env["KARMAX_API_TOKEN"] != "<unset>" {
		t.Fatalf("KARMAX_API_TOKEN = %q, want unset (unchanged behaviour)", env["KARMAX_API_TOKEN"])
	}
}
