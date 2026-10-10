package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/hostpaths"
)

// fakeClaudeThatDumpsArgs records its argv, one argument per line, and writes
// the minimal transcript run() expects.
func fakeClaudeThatDumpsArgs(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	home := t.TempDir()
	argsLog := filepath.Join(t.TempDir(), "args.log")
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
printf '%s\n' "$@" > "$KARMAX_TEST_ARGS_LOG"
slug=$(printf '%s' "$workdir" | tr '/._' '---')
dir="$HOME/.claude/projects/$slug"
mkdir -p "$dir"
echo '{"turn":true}' >> "$dir/$sid.jsonl"
echo "ok"
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", home)
	t.Setenv("KARMAX_TEST_ARGS_LOG", argsLog)
	t.Setenv("KARMAX_HARNESS_ENV_PASSTHROUGH", "KARMAX_TEST_CLAUDE_WORKDIR,KARMAX_TEST_ARGS_LOG")
	return argsLog
}

func runWithMaxTurns(t *testing.T, maxTurns int) []string {
	argsLog := fakeClaudeThatDumpsArgs(t)
	tool, workingDir := newEngineEnvTestTool(t)
	t.Setenv("KARMAX_TEST_CLAUDE_WORKDIR", hostpaths.Resolve(workingDir))
	tool.MaxTurns = maxTurns
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "do the thing", "session_id": "lyzn:task-1", "working_dir": workingDir,
	})
	if err != nil || res.IsError {
		t.Fatalf("Execute: %v %+v", err, res)
	}
	b, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func TestMaxTurnsPassedToCLI(t *testing.T) {
	args := runWithMaxTurns(t, 7)
	for i, a := range args {
		if a == "--max-turns" && i+1 < len(args) && args[i+1] == "7" {
			return
		}
	}
	t.Fatalf("expected --max-turns 7 in %v", args)
}

func TestMaxTurnsZeroOmitsFlag(t *testing.T) {
	for _, a := range runWithMaxTurns(t, 0) {
		if a == "--max-turns" {
			t.Fatal("--max-turns must be absent when unset")
		}
	}
}
