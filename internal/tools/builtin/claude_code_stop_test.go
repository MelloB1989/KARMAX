//go:build !windows

package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/tools"
)

// The Stop button in the desktop app has to reach a `claude` process that
// exec.CommandContext started, plus every shell and command IT started —
// none of which share a Go context, since they are the CLI's own children.
// fakeHangingClaude stands in for a `claude` that has started real work: it
// writes its own pid and a child's pid to two files, then blocks, so a test
// can watch whether a stop actually reaches both.
//
// KARMAX_TEST_IGNORE_TERM makes both the script and its child ignore
// SIGTERM (surviving across exec, per POSIX signal-disposition rules), so a
// test can drive the SIGKILL escalation specifically rather than the
// ordinary SIGTERM path.
func fakeHangingClaude(t *testing.T) (pidFile, childPidFile string) {
	t.Helper()
	binDir := t.TempDir()
	home := t.TempDir()
	pidDir := t.TempDir()
	pidFile = filepath.Join(pidDir, "claude.pid")
	childPidFile = filepath.Join(pidDir, "child.pid")

	script := `#!/bin/bash
if [ -n "${KARMAX_TEST_IGNORE_TERM:-}" ]; then
  trap '' TERM
fi
echo $$ > "$KARMAX_TEST_PIDFILE"
if [ -n "${KARMAX_TEST_IGNORE_TERM:-}" ]; then
  ( trap '' TERM; exec sleep 600 ) &
else
  sleep 600 &
fi
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
	t.Setenv("KARMAX_HARNESS_ENV_PASSTHROUGH",
		"KARMAX_TEST_PIDFILE,KARMAX_TEST_CHILD_PIDFILE,KARMAX_TEST_IGNORE_TERM")
	return pidFile, childPidFile
}

// waitForFile polls until path exists and has content, returning the int it
// parses to (a pid), or fails the test after timeout.
func waitForFile(t *testing.T, path string, timeout time.Duration) int {
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

// processAlive reports whether pid still exists, via the null-signal probe.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// waitUntilDead polls until pid is gone, failing the test if it outlives
// timeout — the grace period a stop is allowed before escalating to
// SIGKILL, plus slack for scheduling.
func waitUntilDead(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d is still alive after %s", pid, timeout)
}

// startHangingRun kicks off tool.Execute in the background against a
// fakeHangingClaude, and returns once both the CLI's own pid and its
// grandchild's pid are on disk — i.e. once a real run is genuinely in
// flight, not merely started.
func startHangingRun(t *testing.T, tool *ClaudeCodeTool, sessionID, workingDir string) (claudePID, childPID int, wait func() (tools.ToolResult, error)) {
	t.Helper()
	pidFile, childPidFile := fakeHangingClaude(t)

	type outcome struct {
		res tools.ToolResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := tool.Execute(context.Background(), map[string]any{
			"prompt": "hang around", "session_id": sessionID, "working_dir": workingDir,
		})
		done <- outcome{res, err}
	}()

	claudePID = waitForFile(t, pidFile, 5*time.Second)
	childPID = waitForFile(t, childPidFile, 5*time.Second)
	return claudePID, childPID, func() (tools.ToolResult, error) {
		o := <-done
		return o.res, o.err
	}
}

// A stop must reach the WHOLE process group a run started — the direct
// `claude` process and any shell/command it spawned — not just the direct
// child exec.CommandContext knows about. Cancelling only that would leave
// the fake's `sleep` grandchild running forever, which is exactly the bug
// this feature exists to fix (see loophost.go's HarnessWith/HarnessForget).
func TestStopKillsBothPIDsAndFailsTheRun(t *testing.T) {
	tool, workingDir := newSessionKeyTestTool(t)
	resolvedDir := hostpaths.Resolve(workingDir)
	const sessionID = "stop-test:both-pids"

	claudePID, childPID, wait := startHangingRun(t, tool, sessionID, workingDir)

	wasRunning, gotDir := StopRun(sessionID, 10*time.Second)
	if !wasRunning {
		t.Fatalf("StopRun reported was_running=false for a session with a run in flight")
	}
	if gotDir != resolvedDir {
		t.Fatalf("StopRun returned working dir %q, want the resolved dir %q", gotDir, resolvedDir)
	}

	res, err := wait()
	if err != nil {
		t.Fatalf("Execute returned a go error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a stopped run must surface as an error result, got %+v", res)
	}

	waitUntilDead(t, claudePID, 6*time.Second)
	waitUntilDead(t, childPID, 6*time.Second)
}

// The stopped-key block must refuse a second run outright — never start the
// CLI at all — so the recipe's next harness step cannot resurrect a task
// the operator just stopped.
func TestSecondRunWithStoppedKeyIsRefusedWithoutStartingTheCLI(t *testing.T) {
	tool, workingDir := newSessionKeyTestTool(t)
	const sessionID = "stop-test:second-run-refused"

	claudePID, childPID, wait := startHangingRun(t, tool, sessionID, workingDir)
	if wasRunning, _ := StopRun(sessionID, 10*time.Second); !wasRunning {
		t.Fatalf("expected a run in flight to stop")
	}
	if _, err := wait(); err != nil {
		t.Fatalf("Execute returned a go error: %v", err)
	}
	waitUntilDead(t, claudePID, 6*time.Second)
	waitUntilDead(t, childPID, 6*time.Second)

	// A second, completely fresh attempt against the same key must be
	// refused immediately — proven by there being no NEW claude process at
	// all: fakeHangingClaude is intentionally not re-armed here, so if
	// run() tried to exec "claude" again it would fail loudly rather than
	// silently succeed, but the real proof is IsError coming back fast
	// with no attempt to reach the CLI.
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "try again", "session_id": sessionID, "working_dir": workingDir,
	})
	if err != nil {
		t.Fatalf("Execute returned a go error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a second run against a stopped key must be refused, got %+v", res)
	}
	if !strings.Contains(strings.ToLower(res.Error), "stop") {
		t.Fatalf("refusal message %q does not name the key as stopped", res.Error)
	}
}

// Stopping a key with nothing running must not be an error — the exact race
// the block exists to close is a stop landing before the recipe's harness
// step has even started the run.
func TestStopWithNothingRunningAnswersNotRunning(t *testing.T) {
	wasRunning, gotDir := StopRun("stop-test:nothing-running", 2*time.Second)
	if wasRunning {
		t.Fatalf("StopRun reported was_running=true for a key nothing ever ran under")
	}
	if gotDir != "" {
		t.Fatalf("StopRun returned a working dir %q for a key nothing ran under, want empty", gotDir)
	}
}

// The existing 10-minute timeout is one of the three ways a run ends early
// (explicit stop, timeout, parent shutdown) and all three must reach the
// whole group the same way. Timeout is made injectable via
// ClaudeCodeTool.Timeout precisely so this can be tested in well under ten
// minutes.
func TestTimeoutKillsTheChildAsWell(t *testing.T) {
	tool, workingDir := newSessionKeyTestTool(t)
	tool.Timeout = 2 * time.Second
	pidFile, childPidFile := fakeHangingClaude(t)

	type outcome struct {
		res tools.ToolResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := tool.Execute(context.Background(), map[string]any{
			"prompt": "hang around", "session_id": "stop-test:timeout", "working_dir": workingDir,
		})
		done <- outcome{res, err}
	}()

	// Confirm the run actually started (both pids on disk) before waiting
	// on the timeout — otherwise a too-short timeout could fire before the
	// fake even gets to write them, and the test would fail for the wrong
	// reason.
	claudePID := waitForFile(t, pidFile, 5*time.Second)
	childPID := waitForFile(t, childPidFile, 5*time.Second)

	o := <-done
	if o.err != nil {
		t.Fatalf("Execute returned a go error: %v", o.err)
	}
	if !o.res.IsError {
		t.Fatalf("a timed-out run must surface as an error result, got %+v", o.res)
	}

	waitUntilDead(t, claudePID, 6*time.Second)
	waitUntilDead(t, childPID, 6*time.Second)
}

// SIGTERM is not guaranteed to work — a child (or a `claude` build) that
// traps or ignores it must still die, by SIGKILL, within the grace period.
func TestChildIgnoringSIGTERMIsStillKilledBySIGKILL(t *testing.T) {
	tool, workingDir := newSessionKeyTestTool(t)
	t.Setenv("KARMAX_TEST_IGNORE_TERM", "1")
	const sessionID = "stop-test:ignores-term"

	claudePID, childPID, wait := startHangingRun(t, tool, sessionID, workingDir)

	wasRunning, _ := StopRun(sessionID, 10*time.Second)
	if !wasRunning {
		t.Fatalf("expected a run in flight to stop")
	}
	if _, err := wait(); err != nil {
		t.Fatalf("Execute returned a go error: %v", err)
	}

	waitUntilDead(t, claudePID, 7*time.Second)
	waitUntilDead(t, childPID, 7*time.Second)
}

// The stopped-key block's clock is injectable — stoppedUntil takes `now`
// as a parameter rather than reading time.Now() itself — so this exercises
// the exact expiry computation StopRun/run() use, without a real 30-minute
// wait.
func TestStoppedBlockExpiresAfterTheWindow(t *testing.T) {
	const sessionID = "stop-test:block-expiry"
	start := time.Now()
	blockKey(sessionID, start)

	if _, blocked := stoppedUntil(sessionID, start.Add(29*time.Minute)); !blocked {
		t.Fatalf("key must still be blocked 29 minutes in")
	}
	if _, blocked := stoppedUntil(sessionID, start.Add(31*time.Minute)); blocked {
		t.Fatalf("key must no longer be blocked 31 minutes in")
	}
}
