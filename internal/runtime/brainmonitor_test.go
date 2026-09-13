package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/MelloB1989/karmax/internal/harness"
)

// fakeBrainSender stands in for *harness.Supervisor: just enough to drive
// brain-monitor's health check without a real CLI process.
type fakeBrainSender struct {
	turn harness.Turn
	err  error
	// kinds records every kind the monitor asked for, so a test can assert it
	// never spends an operator-priority tier on a health check.
	kinds []string
}

func (f *fakeBrainSender) Send(ctx context.Context, key, kind, text string) (harness.Turn, error) {
	f.kinds = append(f.kinds, kind)
	return f.turn, f.err
}

// The harness answering at all — any non-empty reply — is a healthy brain.
func TestBrainMonitorReportsHealthyWhenTheHarnessAnswers(t *testing.T) {
	h := &fakeBrainSender{turn: harness.Turn{Text: "OK"}}
	health, reason := checkBrainHealth(context.Background(), h)
	if health != brainHealthy {
		t.Fatalf("health = %v, want brainHealthy (reason %q)", health, reason)
	}
}

// A real error from the harness — not a breaker trip — is what "down" means.
func TestBrainMonitorReportsDownWhenTheHarnessFails(t *testing.T) {
	h := &fakeBrainSender{err: errors.New("harness process crashed")}
	health, reason := checkBrainHealth(context.Background(), h)
	if health != brainDownState {
		t.Fatalf("health = %v, want brainDownState", health)
	}
	if reason == "" {
		t.Fatal("reason is empty, want the underlying error")
	}
}

// The single most important distinction: an open circuit breaker means the
// daemon is deliberately pausing on quota — the system working as designed —
// and must never be reported as the brain being down.
func TestAnOpenBreakerIsNotReportedAsDown(t *testing.T) {
	h := &fakeBrainSender{err: harness.ErrBreakerOpen{Reason: "5h window is 92% used"}}
	health, reason := checkBrainHealth(context.Background(), h)
	if health == brainDownState {
		t.Fatalf("health = brainDownState, want anything but that for a breaker-open error")
	}
	if health != brainPaused {
		t.Fatalf("health = %v, want brainPaused", health)
	}
	if reason != "5h window is 92% used" {
		t.Fatalf("reason = %q, want the breaker's own reason", reason)
	}
}

// Edge-triggered: a real outage produces exactly one alert while it persists,
// and exactly one recovery message when it clears — never a message every
// tick. A breaker-open reading in between must not reset the latch or alert.
func TestBrainMonitorDoesNotAlertTwiceForOneOutage(t *testing.T) {
	m := &brainMonitorLatch{}
	down := &fakeBrainSender{err: errors.New("connection refused")}

	event, _ := m.tick(context.Background(), down)
	if event != brainMonitorAlertDown {
		t.Fatalf("first failing tick: event = %v, want brainMonitorAlertDown", event)
	}

	event, _ = m.tick(context.Background(), down)
	if event != brainMonitorNone {
		t.Fatalf("second failing tick: event = %v, want brainMonitorNone (no repeat alert)", event)
	}

	event, _ = m.tick(context.Background(), down)
	if event != brainMonitorNone {
		t.Fatalf("third failing tick: event = %v, want brainMonitorNone (no repeat alert)", event)
	}

	// A breaker trip mid-outage is not a recovery and not a fresh down: it
	// must produce no event and must not clear the latch.
	paused := &fakeBrainSender{err: harness.ErrBreakerOpen{Reason: "window exhausted"}}
	event, _ = m.tick(context.Background(), paused)
	if event != brainMonitorNone {
		t.Fatalf("paused tick mid-outage: event = %v, want brainMonitorNone", event)
	}

	healthy := &fakeBrainSender{turn: harness.Turn{Text: "OK"}}
	event, _ = m.tick(context.Background(), healthy)
	if event != brainMonitorAlertRecovered {
		t.Fatalf("recovering tick: event = %v, want brainMonitorAlertRecovered", event)
	}

	// And recovery itself only fires once.
	event, _ = m.tick(context.Background(), healthy)
	if event != brainMonitorNone {
		t.Fatalf("second healthy tick: event = %v, want brainMonitorNone (no repeat recovery)", event)
	}
}

// A breaker-open reading before any outage was ever observed must not be
// mistaken for one: no alert, no latched "down" state.
func TestAnOpenBreakerNeverLatchesDownFromHealthy(t *testing.T) {
	m := &brainMonitorLatch{}
	paused := &fakeBrainSender{err: harness.ErrBreakerOpen{Reason: "quota"}}
	event, _ := m.tick(context.Background(), paused)
	if event != brainMonitorNone {
		t.Fatalf("event = %v, want brainMonitorNone", event)
	}
	if m.down {
		t.Fatal("latch.down = true, want false — a paused breaker is not a down brain")
	}
}

// The health check must never spend an operator-priority kind — that would
// bill a person's own conversation quota for a background health check.
func TestBrainMonitorPingsOnTheCheapestKind(t *testing.T) {
	h := &fakeBrainSender{turn: harness.Turn{Text: "OK"}}
	_, _ = checkBrainHealth(context.Background(), h)
	if len(h.kinds) != 1 {
		t.Fatalf("Send called %d times, want 1", len(h.kinds))
	}
	for _, operatorKind := range []string{"chat", "agent", "task"} {
		if h.kinds[0] == operatorKind {
			t.Fatalf("kind = %q, must not be the operator-priority kind %q", h.kinds[0], operatorKind)
		}
	}
}
