package harness

import (
	"fmt"
	"sync"
	"time"
)

// Breaker decides whether the harness may be used right now.
//
// It reads the quota the harness itself reports rather than inferring one from
// our own spend. That distinction matters here: the five-hour and seven-day
// windows are per ACCOUNT, and the same account runs the operator's interactive
// sessions, which are most of the consumption and entirely invisible to any
// ledger KARMAX keeps. A share computed from our own spend would happily green-
// light a call into a window the operator had already used up.
//
// When it trips, callers fall back to the metered API path. Nothing stops
// working; it costs money and gets slightly worse until the window rolls.
type Breaker struct {
	mu sync.Mutex

	// share is the fraction of each window KARMAX may consume before standing
	// down and leaving the rest for the operator.
	share float64

	last      *RateLimit
	trippedAt time.Time
	reason    string

	// notify announces a trip or a reset. A breaker that changes state in
	// silence is indistinguishable from a feature nobody uses — which is how
	// this class of thing sits broken for weeks.
	notify func(tripped bool, reason string)
}

func NewBreaker(share float64, notify func(bool, string)) *Breaker {
	if share <= 0 || share > 1 {
		share = 0.4
	}
	return &Breaker{share: share, notify: notify}
}

// Observe records what the harness said about the account's quota.
func (b *Breaker) Observe(rl *RateLimit) {
	if rl == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last = rl

	name, worst := rl.Worst()
	switch {
	case rl.Status == "rejected" || rl.Status == "blocked":
		b.trip(fmt.Sprintf("the harness refused: %s", rl.Status))
	case worst.Utilization >= b.share:
		b.trip(fmt.Sprintf("%s window is %.0f%% used, past KARMAX's %.0f%% share",
			name, worst.Utilization*100, b.share*100))
	default:
		b.reset()
	}
}

// TripOn records a hard failure that is not about quota — a missing binary, an
// expired login. These are facts, not estimates, so they trip immediately.
func (b *Breaker) TripOn(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trip(reason)
}

// Allow reports whether a harness call may proceed, and why not when it may not.
func (b *Breaker) Allow() (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.trippedAt.IsZero() {
		return true, ""
	}
	// A quota trip clears itself when the window it named resets; anything else
	// is retried after a cooling-off period rather than being latched forever.
	if b.last != nil {
		if _, worst := b.last.Worst(); worst.ResetsAt > 0 && time.Now().Unix() >= worst.ResetsAt {
			b.reset()
			return true, ""
		}
	}
	if time.Since(b.trippedAt) > 15*time.Minute {
		b.reset()
		return true, ""
	}
	return false, b.reason
}

// Status reports the current windows for `karmax session status`.
func (b *Breaker) Status() (tripped bool, reason string, rl *RateLimit) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.trippedAt.IsZero(), b.reason, b.last
}

// trip and reset must be called with the lock held.
func (b *Breaker) trip(reason string) {
	if !b.trippedAt.IsZero() {
		return // already tripped; do not re-announce
	}
	b.trippedAt = time.Now()
	b.reason = reason
	if b.notify != nil {
		b.notify(true, reason)
	}
}

func (b *Breaker) reset() {
	if b.trippedAt.IsZero() {
		return
	}
	b.trippedAt = time.Time{}
	was := b.reason
	b.reason = ""
	if b.notify != nil {
		b.notify(false, was)
	}
}
