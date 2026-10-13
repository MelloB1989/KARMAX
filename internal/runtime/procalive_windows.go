//go:build windows

package runtime

import "os"

// processAlive: on Windows os.FindProcess opens the process and fails when it
// no longer exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
