//go:build !windows

package proctrack

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// SetGroup starts cmd as the leader of its own process group, so the whole
// tree it spawns can be signalled together.
func SetGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillGroup SIGKILLs the process group led by pid.
func KillGroup(pid int) {
	if pid > 1 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func killGroup(pgid int) { KillGroup(pgid) }

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func processStart(pid int) string {
	s, _ := processStartAlive(pid)
	return s
}

// processStartAlive reports the start time of pid as ps prints it, and whether
// the process exists at all. A zombie counts as gone.
func processStartAlive(pid int) (string, bool) {
	if syscall.Kill(pid, 0) != nil && !errors.Is(syscall.Kill(pid, 0), syscall.EPERM) {
		return "", false
	}
	out, err := exec.Command("ps", "-o", "stat=,lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", false
	}
	f := strings.Fields(line)
	if strings.HasPrefix(f[0], "Z") {
		return "", false
	}
	return strings.Join(f[1:], " "), true
}
