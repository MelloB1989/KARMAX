//go:build !windows

package browser

import (
	"os/exec"
	"syscall"
)

// detach puts the browser in its own process group.
//
// Without it a Ctrl-C in the terminal running KARMAX closes the window someone
// is halfway through signing into, and a daemon restart takes their session
// with it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
