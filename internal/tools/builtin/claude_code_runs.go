package builtin

import (
	"context"
	"sync"
	"time"
)

// ClaudeCodeTool is constructed fresh on every call — HarnessWith,
// HarnessForget, and every other caller build one inline (loophost.go,
// runtime.go, lyzntasks.go, api/graph.go, whatsapp_media.go) — so a record
// of runs currently in flight cannot live on the struct. It lives here
// instead, package-level, guarded by its own mutex.

// runEntry is what a stop needs to interrupt a run and wait for it to
// actually exit.
type runEntry struct {
	cancel     context.CancelFunc
	workingDir string
	done       chan struct{}
}

var (
	runsMu sync.Mutex
	runs   = map[string]*runEntry{}

	stopMu  sync.Mutex
	stopped = map[string]time.Time{} // session_id -> blocked until
)

// stopBlockDuration is how long a stopped key refuses new runs — long
// enough to close the race between a stop landing and the lyzn-tasks
// recipe's next harness step claiming the same key.
const stopBlockDuration = 30 * time.Minute

// registerRun records sessionID — the caller's own identifier, exactly as
// given (a session key like "lyzn:<task id>", or a bare uuid) — as running
// the CLI right now, in workingDir (already resolved), cancellable via
// cancel. A blank sessionID registers nothing: an unkeyed call has no
// identity a stop could ever name.
//
// The returned func must be called exactly once, when the CLI call
// returns: it removes the entry and closes done, unblocking anything
// waiting on this run to exit.
func registerRun(sessionID, workingDir string, cancel context.CancelFunc) (done chan struct{}, unregister func()) {
	done = make(chan struct{})
	if sessionID == "" {
		return done, func() { close(done) }
	}
	entry := &runEntry{cancel: cancel, workingDir: workingDir, done: done}
	runsMu.Lock()
	runs[sessionID] = entry
	runsMu.Unlock()
	return done, func() {
		runsMu.Lock()
		if runs[sessionID] == entry {
			delete(runs, sessionID)
		}
		runsMu.Unlock()
		close(done)
	}
}

// IsRunning reports whether sessionID currently has a claude_code CLI call in
// flight. Read-only, unlike StopRun: a transcript viewer needs to know
// whether a turn is live, not to end it.
func IsRunning(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	runsMu.Lock()
	defer runsMu.Unlock()
	return runs[sessionID] != nil
}

// blockKey marks sessionID as stopped until stopBlockDuration after now.
func blockKey(sessionID string, now time.Time) {
	stopMu.Lock()
	stopped[sessionID] = now.Add(stopBlockDuration)
	stopMu.Unlock()
}

// stoppedUntil reports whether sessionID is still inside its post-stop
// block as of now, and until when. Takes now as a parameter — rather than
// reading time.Now() itself — so a test can drive the 30-minute expiry
// without a real clock; run() and StopRun call it with time.Now().
func stoppedUntil(sessionID string, now time.Time) (time.Time, bool) {
	if sessionID == "" {
		return time.Time{}, false
	}
	stopMu.Lock()
	defer stopMu.Unlock()
	until, ok := stopped[sessionID]
	if !ok || !now.Before(until) {
		return time.Time{}, false
	}
	return until, true
}

// StopRun blocks sessionID from starting any new claude_code run for the
// next 30 minutes, and — when one is in flight — cancels it and waits up
// to wait for it to actually exit. wasRunning reports whether there was
// one; workingDir is that run's own resolved working directory (so a
// caller that does not otherwise know it, such as harness.stop, still
// learns it). Both are zero when nothing was running.
//
// A stop for a key with nothing running is not an error — it still blocks
// the key, which is exactly what closes the race between a stop landing
// and the recipe's next harness step claiming the same key.
func StopRun(sessionID string, wait time.Duration) (wasRunning bool, workingDir string) {
	return stopRun(sessionID, time.Now(), wait)
}

func stopRun(sessionID string, now time.Time, wait time.Duration) (bool, string) {
	if sessionID == "" {
		return false, ""
	}
	blockKey(sessionID, now)

	runsMu.Lock()
	entry := runs[sessionID]
	runsMu.Unlock()
	if entry == nil {
		return false, ""
	}
	entry.cancel()
	select {
	case <-entry.done:
	case <-time.After(wait):
	}
	return true, entry.workingDir
}
