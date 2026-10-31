//go:build !windows

package harness

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the harness in a process group of its own, so Close
// can signal everything that stayed in it at once: the commands, servers and
// hooks a session starts, not only the CLI exec.Cmd knows about. Mirrors
// internal/tools/builtin/procgroup_unix.go.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup signals every process in the group led by pgid.
func signalGroup(pgid int, sig syscall.Signal) error {
	return syscall.Kill(-pgid, sig)
}

// signalProc signals p, and the whole group when p leads one: a shell the CLI
// started in a session of its own takes the command it is running with it.
func signalProc(p proc, sig syscall.Signal) {
	if p.pgid == p.pid {
		_ = syscall.Kill(-p.pid, sig)
		return
	}
	_ = syscall.Kill(p.pid, sig)
}

// groupAlive reports whether any process is left in the group led by pgid.
func groupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }
