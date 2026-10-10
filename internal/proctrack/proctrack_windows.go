//go:build windows

package proctrack

import "os/exec"

// Windows has no process groups to signal here. The simplest safe behaviour is
// to track nothing across restarts: processStartAlive never reports a live
// match and groupAlive is false, so reap never kills anything.

// SetGroup is a no-op.
func SetGroup(cmd *exec.Cmd) {}

// KillGroup is a no-op; callers kill the direct process themselves.
func KillGroup(pid int) {}

func killGroup(pgid int)                       {}
func groupAlive(pgid int) bool                 { return false }
func processStart(pid int) string              { return "" }
func processStartAlive(pid int) (string, bool) { return "", true }
