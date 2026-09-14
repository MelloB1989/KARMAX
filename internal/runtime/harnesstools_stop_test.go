//go:build !windows

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/chatlog"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"go.uber.org/zap"
)

// fakeHangingClaudeForStop mirrors internal/tools/builtin's own
// fakeHangingClaude — duplicated because Go test helpers do not cross
// package boundaries (see that package's claude_code_stop_test.go). Writes
// its own pid and a backgrounded child's pid to two files, then blocks, so
// harness.stop's kill can be observed end to end through the real tool.
func fakeHangingClaudeForStop(t *testing.T) (home, pidFile, childPidFile string) {
	t.Helper()
	binDir := t.TempDir()
	home = t.TempDir()
	pidDir := t.TempDir()
	pidFile = filepath.Join(pidDir, "claude.pid")
	childPidFile = filepath.Join(pidDir, "child.pid")

	script := `#!/bin/bash
echo $$ > "$KARMAX_TEST_PIDFILE"
sleep 600 &
child=$!
echo $child > "$KARMAX_TEST_CHILD_PIDFILE"
wait $child
exit 1
`
	path := filepath.Join(binDir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", home)
	t.Setenv("KARMAX_TEST_PIDFILE", pidFile)
	t.Setenv("KARMAX_TEST_CHILD_PIDFILE", childPidFile)
	t.Setenv("KARMAX_HARNESS_ENV_PASSTHROUGH", "KARMAX_TEST_PIDFILE,KARMAX_TEST_CHILD_PIDFILE")
	return home, pidFile, childPidFile
}

func waitForPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				n, err := strconv.Atoi(s)
				if err != nil {
					t.Fatalf("pid file %s has non-numeric content %q: %v", path, s, err)
				}
				return n
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return 0
}

func pidIsDeadWithin(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d is still alive after %s", pid, timeout)
}

func newHarnessStopTestKit(t *testing.T) (*KarmaxRuntime, string) {
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
	return rt, filepath.Join("lyzn-tasks", "task-stop")
}

// harness.stop is what the desktop app's Stop button calls. This exercises
// it end to end: a real run in flight (both the direct process and a
// backgrounded grandchild), a pre-existing session-key mapping and
// transcript the way a real earlier turn would have left them, and asserts
// the whole chain — process group killed, run failed, mapping/transcript/
// workdir gone, was_running true — the same cleanup HarnessForget does.
func TestHarnessStopToolStopsARunningSessionAndCleansUp(t *testing.T) {
	home, pidFile, childPidFile := fakeHangingClaudeForStop(t)
	rt, workingDir := newHarnessStopTestKit(t)
	resolved := hostpaths.Resolve(workingDir)
	const sessionID = "lyzn:task-stop"

	seededUUID := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	if err := rt.store.SaveSessionKey(sessionID, seededUUID, "claude_code"); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	transcriptDir := filepath.Join(home, ".claude", "projects", chatlog.Slug(resolved))
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("seed transcript dir: %v", err)
	}
	transcript := filepath.Join(transcriptDir, seededUUID+".jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}

	runningTool := &builtin.ClaudeCodeTool{Store: rt.store}
	type outcome struct {
		res tools.ToolResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := runningTool.Execute(context.Background(), map[string]any{
			"prompt": "hang around", "session_id": sessionID, "working_dir": workingDir,
		})
		done <- outcome{res, err}
	}()

	claudePID := waitForPIDFile(t, pidFile, 5*time.Second)
	childPID := waitForPIDFile(t, childPidFile, 5*time.Second)

	stopTool := &harnessStopTool{ref: &harnessRef{rt: rt}}
	res, err := stopTool.Execute(context.Background(), map[string]any{"session_id": sessionID})
	if err != nil {
		t.Fatalf("harness.stop returned a go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("harness.stop returned an error result: %+v", res)
	}
	out, ok := res.Output.(map[string]any)
	if !ok {
		t.Fatalf("harness.stop output is not a map: %+v", res.Output)
	}
	if out["was_running"] != true {
		t.Fatalf("was_running = %v, want true", out["was_running"])
	}

	o := <-done
	if o.err != nil {
		t.Fatalf("Execute returned a go error: %v", o.err)
	}
	if !o.res.IsError {
		t.Fatalf("a stopped run must surface as an error result, got %+v", o.res)
	}

	pidIsDeadWithin(t, claudePID, 6*time.Second)
	pidIsDeadWithin(t, childPID, 6*time.Second)

	if mapped, err := rt.store.GetSessionKey(sessionID); err != nil || mapped != "" {
		t.Fatalf("mapping still exists after harness.stop: %q, err=%v", mapped, err)
	}
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatalf("transcript still exists after harness.stop: %v", err)
	}
	if _, err := os.Stat(resolved); !os.IsNotExist(err) {
		t.Fatalf("workdir still exists after harness.stop: %v", err)
	}
}

// A stop for a key nothing is running under must not be an error — this is
// exactly the race the block exists to close: a stop landing before the
// recipe's harness step has even started the run.
func TestHarnessStopToolWithNothingRunningAnswersFalse(t *testing.T) {
	rt, _ := newHarnessStopTestKit(t)
	stopTool := &harnessStopTool{ref: &harnessRef{rt: rt}}

	res, err := stopTool.Execute(context.Background(), map[string]any{"session_id": "lyzn:nothing-here"})
	if err != nil {
		t.Fatalf("harness.stop returned a go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("a stop for a key with nothing running must not be an error, got %+v", res)
	}
	out, ok := res.Output.(map[string]any)
	if !ok {
		t.Fatalf("harness.stop output is not a map: %+v", res.Output)
	}
	if out["was_running"] != false {
		t.Fatalf("was_running = %v, want false", out["was_running"])
	}
}

// session_id is the one required input — refuse a blank one outright,
// rather than blocking every future claude_code call with an empty key.
func TestHarnessStopToolRequiresASessionID(t *testing.T) {
	rt, _ := newHarnessStopTestKit(t)
	stopTool := &harnessStopTool{ref: &harnessRef{rt: rt}}

	res, err := stopTool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("harness.stop returned a go error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("harness.stop with no session_id must be an error, got %+v", res)
	}
}
