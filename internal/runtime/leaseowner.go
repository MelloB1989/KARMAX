package runtime

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Who holds a loop lease.
//
// A lease used to be owned by a bare run id, which says nothing about whether
// the process that took it is still there. After a crash the lease therefore
// stayed "held" for its whole TTL (~15 minutes) and every tick answered "loop
// already running; skipping". The owner now names the process, so a starting
// engine can tell a lease left by a dead process (clear it at once) from one
// held by a live concurrent process (never touch it).
//
// Format: <instance>|<pid>|<host>|<run id>. The instance is minted per process
// start; the pid is only used to ask the OS whether that process still exists.

// processInstance identifies this process start. A variable so tests can play
// the part of a second process.
var processInstance = uuid.NewString()

// processHost is this machine's name, as recorded in lease owners.
var processHost = func() string {
	h, _ := os.Hostname()
	return strings.ReplaceAll(h, "|", "_")
}()

func leaseOwnerFor(runID string) string {
	return fmt.Sprintf("%s|%d|%s|%s", processInstance, os.Getpid(), processHost, runID)
}

// leaseOwnerParts splits an owner. ok is false for anything not in the format,
// such as a lease written by an older build.
func leaseOwnerParts(owner string) (instance string, pid int, host, runID string, ok bool) {
	p := strings.SplitN(owner, "|", 4)
	if len(p) != 4 {
		return "", 0, "", "", false
	}
	n, err := strconv.Atoi(p[1])
	if err != nil {
		return "", 0, "", "", false
	}
	return p[0], n, p[2], p[3], true
}

// leaseHolderAlive says whether the process behind a lease owner may still be
// running. It errs toward "alive": a wrongly kept lease only costs the TTL,
// a wrongly stolen one runs a loop twice.
func leaseHolderAlive(owner string) bool {
	instance, pid, host, _, ok := leaseOwnerParts(owner)
	if !ok {
		// Not written by this build, so it cannot belong to this process.
		return false
	}
	if instance == processInstance {
		return true
	}
	if host != processHost {
		return true // another machine sharing the database: cannot tell
	}
	if pid == os.Getpid() {
		return false // our pid, someone else's instance: the pid was reused
	}
	return processAlive(pid)
}
