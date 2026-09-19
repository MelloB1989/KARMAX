//go:build !windows

package builtin

import (
	"os/exec"
	"syscall"
	"time"
)

// setupProcessGroup puts a spawned `claude` in its own process group, so a
// stop can reach the shells and commands it started, not just the direct
// process exec.CommandContext knows about. Mirrors
// internal/browser/detach_unix.go, for the same reason.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup is cmd.Cancel: it runs when the run's context ends,
// whichever of the three ways that happens — an explicit stop, the
// timeout, or the parent context (engine shutdown). SIGTERM goes to the
// whole process group first; SIGKILL follows 5 seconds later if the group
// hasn't exited by then.
//
// cmd.WaitDelay is set to the same 5 seconds so CombinedOutput cannot block
// forever on a grandchild holding the output pipe open — but WaitDelay's
// own kill only ever reaches cmd.Process, the direct child. Escalating the
// whole group to SIGKILL is this function's job, via the goroutine below,
// not WaitDelay's.
func terminateGroup(cmd *exec.Cmd, done <-chan struct{}) error {
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	go func() {
		select {
		case <-done:
			// The run already exited; nothing left to escalate against.
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}()
	return nil
}
