package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"go.uber.org/zap"
)

func screenedEvent(meta map[string]string) bus.Event {
	return bus.Event{
		ID: "evt-1", Kind: bus.EventCommsMessage, AgentID: "a",
		Timestamp: time.Now(), Meta: meta,
	}
}

func TestScreeningOfReadsTheVerdict(t *testing.T) {
	s := screeningOf(screenedEvent(map[string]string{
		bus.MetaReflexAction:   "delegate",
		bus.MetaReflexEffort:   "heavy",
		bus.MetaReflexUrgency:  "0.75",
		bus.MetaReflexRisk:     "0.50",
		bus.MetaReflexApproval: "required",
		bus.MetaReflexReason:   "needs the shell",
	}))
	if !s.Screened {
		t.Fatal("an annotated event should read as screened")
	}
	if s.Action != "delegate" || s.Effort != "heavy" {
		t.Errorf("action/effort = %q/%q", s.Action, s.Effort)
	}
	if s.Urgency != 0.75 || s.Risk != 0.50 {
		t.Errorf("urgency/risk = %v/%v", s.Urgency, s.Risk)
	}
	if !s.Approval {
		t.Error("approval should be required")
	}
}

// An unscreened event must be indistinguishable from one that arrived before
// reflex existed, or turns would silently change behaviour when it is off.
func TestUnscreenedEventYieldsNothing(t *testing.T) {
	for name, evt := range map[string]bus.Event{
		"no meta":    screenedEvent(nil),
		"empty meta": screenedEvent(map[string]string{}),
		"other meta": screenedEvent(map[string]string{"unrelated": "x"}),
	} {
		s := screeningOf(evt)
		if s.Screened {
			t.Errorf("%s: should not read as screened", name)
		}
		if s.context() != "" {
			t.Errorf("%s: should contribute no context", name)
		}
	}
}

func TestScreeningContextSteersTheTurn(t *testing.T) {
	escalate := screeningOf(screenedEvent(map[string]string{bus.MetaReflexAction: "escalate"})).context()
	if !strings.Contains(escalate, "TRIAGE") || !strings.Contains(escalate, "time-critical") {
		t.Errorf("escalate context = %q", escalate)
	}
	delegate := screeningOf(screenedEvent(map[string]string{bus.MetaReflexAction: "delegate"})).context()
	if !strings.Contains(delegate, "claude_code") {
		t.Errorf("delegate context = %q", delegate)
	}
	approval := screeningOf(screenedEvent(map[string]string{
		bus.MetaReflexAction: "handle", bus.MetaReflexApproval: "required",
	})).context()
	if !strings.Contains(approval, "approval") {
		t.Errorf("approval context = %q", approval)
	}
	// A plain handle is the common case and must add nothing at all.
	if got := screeningOf(screenedEvent(map[string]string{bus.MetaReflexAction: "handle"})).context(); got != "" {
		t.Errorf("a plain handle added context: %q", got)
	}
}

func TestScreenPassesThroughWithoutAScreener(t *testing.T) {
	a := &Agent{log: zap.NewNop()}
	evt := screenedEvent(nil)
	got, ok := a.screen(evt)
	if !ok {
		t.Fatal("an agent with no screener must handle every event")
	}
	if got.ID != evt.ID {
		t.Error("the event should pass through untouched")
	}
}

func TestScreenHonoursTheVerdict(t *testing.T) {
	a := &Agent{log: zap.NewNop(), ctx: context.Background()}
	a.SetScreener(func(_ context.Context, e bus.Event) (bus.Event, bool) {
		return e, false
	})
	if _, ok := a.screen(screenedEvent(nil)); ok {
		t.Fatal("a screener that said no should stop the event")
	}
}

// The screener is what annotates, so what it returns has to be what the turn
// reads — not the event it was handed.
func TestScreenReturnsTheAnnotatedEvent(t *testing.T) {
	a := &Agent{log: zap.NewNop(), ctx: context.Background()}
	a.SetScreener(func(_ context.Context, e bus.Event) (bus.Event, bool) {
		e.Meta = map[string]string{
			bus.MetaReflexAction: "handle", bus.MetaReflexEffort: "trivial",
		}
		return e, true
	})
	got, ok := a.screen(screenedEvent(nil))
	if !ok {
		t.Fatal("expected the event to proceed")
	}
	if screeningOf(got).Effort != "trivial" {
		t.Errorf("annotation was lost: %v", got.Meta)
	}
}

// brainFor is the token saving. It must only redirect on an explicit trivial
// verdict, and only when there is somewhere cheaper to send it.
func TestBrainForKeepsTheHarnessWithoutATrivialVerdict(t *testing.T) {
	a := &Agent{log: zap.NewNop()}
	hb := &MainModelSession{}
	a.harnessBrain = hb
	a.mainSession = &MainModelSession{}

	for name, evt := range map[string]bus.Event{
		"unscreened": screenedEvent(nil),
		"normal":     screenedEvent(map[string]string{bus.MetaReflexAction: "handle", bus.MetaReflexEffort: "normal"}),
		"heavy":      screenedEvent(map[string]string{bus.MetaReflexAction: "delegate", bus.MetaReflexEffort: "heavy"}),
	} {
		if got := a.brainFor(evt); got != Brain(hb) {
			t.Errorf("%s: should stay on the harness brain", name)
		}
	}
}

func TestBrainForRoutesTrivialToTheAPISession(t *testing.T) {
	a := &Agent{log: zap.NewNop()}
	a.harnessBrain = &MainModelSession{}
	sess := &MainModelSession{}
	a.mainSession = sess

	evt := screenedEvent(map[string]string{
		bus.MetaReflexAction: "handle", bus.MetaReflexEffort: "trivial",
	})
	if got := a.brainFor(evt); got != Brain(sess) {
		t.Error("a trivial verdict should route to the cheaper API session")
	}
}

// With no harness wired there is nothing to save, and no second engine to pick.
func TestBrainForWithoutAHarnessIsUnchanged(t *testing.T) {
	a := &Agent{log: zap.NewNop()}
	sess := &MainModelSession{}
	a.mainSession = sess
	evt := screenedEvent(map[string]string{
		bus.MetaReflexAction: "handle", bus.MetaReflexEffort: "trivial",
	})
	if got := a.brainFor(evt); got != Brain(sess) {
		t.Error("should fall through to the only brain there is")
	}
}

// When the API session is Claude Code too, a trivial verdict has nowhere
// cheaper to go — redirecting only sends the message to a context-less second
// session. It stays with the agent.
func TestBrainForKeepsTrivialOnTheAgentWhenEverythingIsClaudeCode(t *testing.T) {
	a := &Agent{log: zap.NewNop()}
	hb := &MainModelSession{}
	a.harnessBrain = hb
	a.mainSession = &MainModelSession{provider: "claude-code"}
	evt := screenedEvent(map[string]string{
		bus.MetaReflexAction: "handle", bus.MetaReflexEffort: "trivial",
	})
	if got := a.brainFor(evt); got != Brain(hb) {
		t.Fatal("a trivial turn left the agent for a Claude Code side session")
	}
}
