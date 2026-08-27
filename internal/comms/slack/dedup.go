package slack

import (
	"sync"
	"time"
)

// envelopeWindow bounds how long an envelope/event id is remembered. Slack's
// own redelivery window is measured in seconds; this outlives it comfortably
// without growing without bound on a connection that stays up for days.
const envelopeWindow = 10 * time.Minute

// envelopeDedup remembers delivery ids it has already processed, so a
// redelivered socket envelope or a retried Events API webhook is not run
// twice. Same reserve-and-sweep-on-access shape as comms.sendGuard (see
// internal/comms/dedup.go) — that one guards outbound content, this one
// guards inbound delivery ids, but neither needs more than a map and a mutex.
type envelopeDedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newEnvelopeDedup() *envelopeDedup {
	return &envelopeDedup{seen: make(map[string]time.Time)}
}

// seenBefore records id at now and reports whether it was already recorded.
// An empty id never counts as seen — refusing to dedupe is safer than
// dropping a message because nothing gave us a key to work with.
func (d *envelopeDedup) seenBefore(id string, now time.Time) bool {
	if id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, at := range d.seen {
		if now.Sub(at) > envelopeWindow {
			delete(d.seen, k)
		}
	}
	if _, ok := d.seen[id]; ok {
		return true
	}
	d.seen[id] = now
	return false
}
