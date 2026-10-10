//go:build !windows

package runtime

import (
	"errors"
	"syscall"
)

// processAlive asks the OS whether a process with this pid exists. EPERM means
// it exists but belongs to someone else, which is still alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
