package target

import (
	"os/exec"
	"syscall"
	"time"
)

// WaitDelay bounds how long Wait lingers for a killed command's pipes to
// close. `docker --context pc2 …` runs ssh as a child, and ssh can keep the
// stdout and stderr pipes open after docker is killed; without a bound,
// cmd.Wait blocks on the copy goroutines for as long as ssh lives.
const WaitDelay = 5 * time.Second

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

// HardenCtx is Harden for a command made by exec.CommandContext: when its
// context ends, the whole group is killed, and Wait gives up on pipes that
// grandchildren hold after WaitDelay.
func HardenCtx(cmd *exec.Cmd) *exec.Cmd {
	Harden(cmd)
	cmd.Cancel = func() error { return KillGroup(cmd) }
	return cmd
}
