//go:build windows

package harness

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup is a no-op on windows, which has no unix process groups:
// Close reaches the CLI alone, as it always did. Mirrors
// internal/tools/builtin/procgroup_windows.go.
func setProcessGroup(*exec.Cmd) {}

// signalGroup signals the one process: there is no group to reach.
func signalGroup(pid int, sig syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if sig == syscall.SIGKILL || sig == syscall.SIGTERM {
		return p.Kill()
	}
	return p.Signal(sig)
}

func signalProc(p proc, sig syscall.Signal) { _ = signalGroup(p.pid, sig) }

// groupAlive is false: with no group, there is nothing left to find.
func groupAlive(int) bool { return false }
