package builtin

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The karmax-* skills installed for the harness (~/.claude/skills) tell it to
// run bare `karmax browser ...` commands. The running engine binary is never
// named that — karmax-darwin-arm64, or Resources/core/karmax-<os>-<arch> in
// the packaged app — and its directory isn't on the harness's PATH, so
// without this, every such command fails "command not found". This is the
// generic fix: a stable `karmax` entry under <DataDir>/bin, pointing at the
// currently-running engine binary, prepended onto the allowlisted PATH the
// harness child gets.

// TestClaudeCodeToolHarnessCmdEnvPutsKarmaxOnPATH is the (a) half: after the
// harness env is built for a ClaudeCodeTool with a DataDir set, `karmax`
// must be resolvable via THAT env's own PATH (not the test binary's ambient
// one) and must resolve to the currently-running engine executable.
func TestClaudeCodeToolHarnessCmdEnvPutsKarmaxOnPATH(t *testing.T) {
	dataDir := t.TempDir()
	tool := &ClaudeCodeTool{DataDir: dataDir}

	env := tool.harnessCmdEnv()

	wantBinDir := filepath.Join(dataDir, "bin")
	path, ok := lookupEnvVar(env, "PATH")
	if !ok {
		t.Fatalf("built env has no PATH at all: %v", env)
	}
	dirs := strings.Split(path, string(os.PathListSeparator))
	if len(dirs) == 0 || dirs[0] != wantBinDir {
		t.Fatalf("PATH = %q, want it to start with %q", path, wantBinDir)
	}

	// Resolve `karmax` using exactly the env the harness child would get,
	// not whatever this test process's own PATH happens to hold.
	t.Setenv("PATH", path)
	resolved, err := exec.LookPath("karmax")
	if err != nil {
		t.Fatalf("karmax not resolvable on the harness child's PATH (%q): %v", path, err)
	}

	wantExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if real, err := filepath.EvalSymlinks(wantExe); err == nil {
		wantExe = real
	}

	if runtime.GOOS == "windows" {
		// karmax.cmd is a shim (windows symlinks need privilege this process
		// can't assume), so check its body names the engine binary rather
		// than resolving it as a symlink.
		body, err := os.ReadFile(resolved)
		if err != nil {
			t.Fatalf("read shim %s: %v", resolved, err)
		}
		if !strings.Contains(string(body), wantExe) {
			t.Fatalf("shim %s does not reference engine binary %q:\n%s", resolved, wantExe, body)
		}
		return
	}

	gotExe := resolved
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		gotExe = real
	}
	if gotExe != wantExe {
		t.Fatalf("karmax resolves to %q, want the running engine binary %q", gotExe, wantExe)
	}
}

// TestClaudeCodeToolHarnessCmdEnvLeavesRestOfEnvUnchanged is the (b) half:
// harnessCmdEnv must only ever touch PATH. Every other variable
// safety.HarnessEnv() allowed through comes out with the exact same value,
// and no key is added or dropped besides PATH's own prefix.
func TestClaudeCodeToolHarnessCmdEnvLeavesRestOfEnvUnchanged(t *testing.T) {
	dataDir := t.TempDir()
	tool := &ClaudeCodeTool{DataDir: dataDir}

	before := harnessEnv()
	after := tool.harnessCmdEnv()

	beforeSet := envMap(before)
	afterSet := envMap(after)

	if len(afterSet) != len(beforeSet) {
		t.Fatalf("env key count changed: before %d %v, after %d %v", len(beforeSet), beforeSet, len(afterSet), afterSet)
	}
	for k, v := range beforeSet {
		got, ok := afterSet[k]
		if !ok {
			t.Fatalf("%s dropped from the env harnessCmdEnv built", k)
		}
		if k == "PATH" {
			wantBinDir := filepath.Join(dataDir, "bin")
			if got != wantBinDir+string(os.PathListSeparator)+v && !(v == "" && got == wantBinDir) {
				t.Fatalf("PATH = %q, want %q prepended to the original %q", got, wantBinDir, v)
			}
			continue
		}
		if got != v {
			t.Fatalf("%s changed: %q -> %q (harnessCmdEnv must only touch PATH)", k, v, got)
		}
	}
}

// TestClaudeCodeToolHarnessCmdEnvIdempotent covers the "refresh, don't fail
// if it already exists" half of the fix: calling harnessCmdEnv twice (as
// every successive harness turn does) must not error and must land on the
// same PATH both times.
func TestClaudeCodeToolHarnessCmdEnvIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	tool := &ClaudeCodeTool{DataDir: dataDir}

	first, ok1 := lookupEnvVar(tool.harnessCmdEnv(), "PATH")
	second, ok2 := lookupEnvVar(tool.harnessCmdEnv(), "PATH")
	if !ok1 || !ok2 {
		t.Fatalf("PATH missing from one of the two calls: ok1=%v ok2=%v", ok1, ok2)
	}
	if first != second {
		t.Fatalf("harnessCmdEnv's PATH changed across calls: %q -> %q", first, second)
	}
}

func lookupEnvVar(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):], true
		}
	}
	return "", false
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}
