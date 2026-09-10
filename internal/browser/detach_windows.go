//go:build windows

package browser

import (
	"os/exec"
	"syscall"
)

// detach gives the browser its own process group, so a console signal sent to
// KARMAX does not close a window someone is signing into.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
