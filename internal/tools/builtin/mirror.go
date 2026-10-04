package builtin

import "sync"

const (
	MirrorNotification = "notification"
	MirrorApproval     = "approval"
)

// MirrorEvent is an operator-facing item that already reached the phone app.
type MirrorEvent struct {
	Kind       string
	Title      string
	Body       string
	Source     string
	ProposalID string
}

var (
	mirrorMu   sync.RWMutex
	mirrorHook func(MirrorEvent)
)

// SetMirrorHook installs the single observer of operator notifications and approvals.
func SetMirrorHook(fn func(MirrorEvent)) {
	mirrorMu.Lock()
	mirrorHook = fn
	mirrorMu.Unlock()
}

func notifyMirror(ev MirrorEvent) {
	mirrorMu.RLock()
	fn := mirrorHook
	mirrorMu.RUnlock()
	if fn != nil {
		fn(ev)
	}
}
