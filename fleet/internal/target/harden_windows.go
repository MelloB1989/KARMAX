//go:build windows

package target

import "os/exec"

// fleetctl on Windows is a client of fleetd: what it runs itself (attach,
// image builds) is a fleet host's work. These keep it building there.

// KillGroup kills the command; Windows has no process groups to kill.
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// Harden bounds Wait.
func Harden(cmd *exec.Cmd) *exec.Cmd {
	cmd.WaitDelay = WaitDelay
	return cmd
}
