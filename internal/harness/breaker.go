package harness

import (
	"fmt"
	"sync"
	"time"
)

// Breaker decides whether the harness may be used right now, and on which tier.
//
// It reads the quota the harness itself reports rather than inferring one from
// our own spend. That distinction matters here: the five-hour and seven-day
// windows are per ACCOUNT, and the same account runs the operator's interactive
// sessions, which are most of the consumption and entirely invisible to any
// ledger KARMAX keeps. A share computed from our own spend would happily green-
// light a call into a window the operator had already used up.
//
// There are two outcomes, not one, and the difference is the whole design.
// Running past KARMAX's share DEGRADES it to the cheapest model rather than
// stopping it: the harness is the only engine now, so refusing is not falling
// back to something slower, it is going silent. Only the account itself saying
// no is a hard stop, and even then the caller queues the work rather than
// dropping it.
type Breaker struct {
	mu sync.Mutex

	// share is the fraction of each window KARMAX may consume before dropping
	// to the cheap tier and leaving the rest of the good models for the
	// operator.
	share float64

	last *RateLimit

	// hard is the account refusing outright. Nothing runs until it clears.
	hardAt time.Time
	// soft is being past our share: everything still runs, on the cheap tier.
	soft   bool
	reason string

	// notify announces a change of state. A breaker that changes state in
	// silence is indistinguishable from a feature nobody uses — which is how
	// this class of thing sits broken for weeks.
	notify func(tripped bool, reason string)
}

func NewBreaker(share float64, notify func(bool, string)) *Breaker {
	if share <= 0 || share > 1 {
		share = 0.85
	}
	return &Breaker{share: share, notify: notify}
}

// Decision is what a caller should do with this turn.
type Decision struct {
	// Allow is false only when the account is refusing. The caller must queue
	// the work and tell the operator, never discard it.
	Allow bool
	// Degrade asks for the cheapest model. The turn still happens.
	Degrade bool
	Reason  string
}

// Decide reports whether to run, and on which tier.
func (b *Breaker) Decide() Decision {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.hardAt.IsZero() {
		// A hard stop clears when the window it named resets, or after a
		// cooling-off period — latching it forever would outlive the cause.
		if b.windowRolled() || time.Since(b.hardAt) > 15*time.Minute {
			b.clear()
		} else {
			return Decision{Allow: false, Reason: b.reason}
		}
	}
	return Decision{Allow: true, Degrade: b.soft, Reason: b.reason}
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
		b.hard(fmt.Sprintf("the account refused: %s", rl.Status))
	case worst.Utilization >= b.share:
		b.degrade(fmt.Sprintf("%s window is %.0f%% used, past KARMAX's %.0f%% share — running on the cheap model",
			name, worst.Utilization*100, b.share*100))
	default:
		b.clear()
	}
}

// TripOn records a hard failure that is not about quota — a missing binary, an
// expired login. These are facts, not estimates, so they stop everything.
func (b *Breaker) TripOn(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hard(reason)
}

// Allow reports whether a call may proceed at all, ignoring the tier.
func (b *Breaker) Allow() (bool, string) {
	d := b.Decide()
	return d.Allow, d.Reason
}

// Status reports the current state for `karmax session list`.
func (b *Breaker) Status() (tripped bool, reason string, rl *RateLimit) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.hardAt.IsZero() || b.soft, b.reason, b.last
}

// windowRolled reports whether the window the breaker named has since reset.
// Must be called with the lock held.
func (b *Breaker) windowRolled() bool {
	if b.last == nil {
		return false
	}
	_, worst := b.last.Worst()
	return worst.ResetsAt > 0 && time.Now().Unix() >= worst.ResetsAt
}

// hard, degrade and clear must be called with the lock held.
func (b *Breaker) hard(reason string) {
	if !b.hardAt.IsZero() {
		return // already stopped; do not re-announce
	}
	b.hardAt = time.Now()
	b.reason = reason
	b.announce(true, reason)
}

func (b *Breaker) degrade(reason string) {
	b.reason = reason
	if b.soft {
		return
	}
	b.soft = true
	b.announce(true, reason)
}

func (b *Breaker) clear() {
	if b.hardAt.IsZero() && !b.soft {
		return
	}
	was := b.reason
	b.hardAt, b.soft, b.reason = time.Time{}, false, ""
	b.announce(false, was)
}

func (b *Breaker) announce(tripped bool, reason string) {
	if b.notify != nil {
		b.notify(tripped, reason)
	}
}
