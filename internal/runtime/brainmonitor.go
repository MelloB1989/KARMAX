package runtime

import (
	"context"
	"strings"

	"github.com/MelloB1989/karmax/internal/harness"
)

// brainMonitorSender is the minimal harness surface the health check needs —
// a plain ping-and-reply, not the whole Supervisor — so a test can fake it
// without a real CLI process. *harness.Supervisor satisfies this directly.
type brainMonitorSender interface {
	Send(ctx context.Context, key, kind, text string) (harness.Turn, error)
}

// brainHealth is what one health check learned.
type brainHealth int

const (
	brainHealthy brainHealth = iota
	// brainPaused means the breaker is open: the daemon is deliberately
	// declining to spend quota right now. That is the system working as
	// designed, not an outage, and callers must never conflate the two.
	brainPaused
	brainDownState
)

// brainMonitorKey and brainMonitorKind name the session the health check
// rides. The kind is deliberately the cheapest background tier — see the
// tier table in docs/CLAUDE-ONLY-ORCHESTRATOR.md — never an operator-
// priority kind ("chat"/"agent"/"task"), which would spend a share of the
// account's own quota on a call nobody asked for.
const (
	brainMonitorKey  = "brain-monitor"
	brainMonitorKind = "classify"
)

const brainMonitorPrompt = "Reply with the single word OK."

// checkBrainHealth pings the harness once and classifies the result.
//
// harnesstools.go's asBreakerOpen shows the shape this borrows: an
// ErrBreakerOpen is not the harness failing, it is quota policy declining
// the call on purpose, and that must never be reported the same way a real
// failure is.
func checkBrainHealth(ctx context.Context, h brainMonitorSender) (brainHealth, string) {
	turn, err := h.Send(ctx, brainMonitorKey, brainMonitorKind, brainMonitorPrompt)
	if err != nil {
		var open harness.ErrBreakerOpen
		if asBreakerOpen(err, &open) {
			return brainPaused, open.Reason
		}
		return brainDownState, err.Error()
	}
	if strings.TrimSpace(turn.Text) == "" {
		return brainDownState, "empty reply"
	}
	return brainHealthy, ""
}

// brainMonitorEvent is what one tick decided the caller should do.
type brainMonitorEvent int

const (
	brainMonitorNone brainMonitorEvent = iota
	brainMonitorAlertDown
	brainMonitorAlertRecovered
)

// brainMonitorLatch edge-triggers the down/recovered alert across ticks, so
// one outage produces exactly one alert and one recovery message — not a
// message on every ten-minute tick it persists for.
type brainMonitorLatch struct{ down bool }

// tick runs one health check and reports what changed, if anything.
//
// A paused (breaker-open) reading never touches the latch and never produces
// an event: it is neither a failure to alert on nor a recovery to announce.
// Reporting it as either would replace one false alarm (pinging a dead API)
// with another that fires on exactly the schedule quota pauses are likeliest
// to happen on.
func (m *brainMonitorLatch) tick(ctx context.Context, h brainMonitorSender) (brainMonitorEvent, string) {
	health, reason := checkBrainHealth(ctx, h)
	switch health {
	case brainPaused:
		return brainMonitorNone, reason
	case brainDownState:
		if m.down {
			return brainMonitorNone, reason
		}
		m.down = true
		return brainMonitorAlertDown, reason
	default: // brainHealthy
		if !m.down {
			return brainMonitorNone, reason
		}
		m.down = false
		return brainMonitorAlertRecovered, reason
	}
}
