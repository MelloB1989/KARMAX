//go:build windows

package builtin

import "os/exec"

// setupProcessGroup is a no-op on windows: exec.Cmd has no equivalent of a
// unix process group here, so a stop falls back to killing the direct
// process only — see terminateGroup. Mirrors
// internal/browser/detach_windows.go's platform split.
func setupProcessGroup(cmd *exec.Cmd) {}

// terminateGroup kills the direct process. There is no portable way to
// reach grandchildren on windows without job objects, which this does not
// set up.
func terminateGroup(cmd *exec.Cmd, done <-chan struct{}) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
