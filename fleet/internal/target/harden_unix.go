//go:build unix

package target

import (
	"os/exec"
	"syscall"
)

// KillGroup kills cmd's process group, so a command's children (ssh under
// docker, tail under docker exec) die with it. The command must have been
// started with Harden or HardenCtx, which make it a group leader; if the
// group is already gone it falls back to the process.
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// Harden makes a command that is stopped by hand (KillGroup) a process-group
// leader with a bounded Wait. Not for commands that need the terminal: a
// group of its own is not the terminal's foreground group.
func Harden(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.WaitDelay = WaitDelay
	return cmd
}
