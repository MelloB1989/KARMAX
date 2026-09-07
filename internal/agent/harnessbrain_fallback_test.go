package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
)

// decliningSender is a harness that will not take the turn — a tripped
// breaker, an exhausted window, expired auth. The routine case, not an outage.
type decliningSender struct{ reason string }

func (d decliningSender) Send(context.Context, string, string, string) (HarnessTurn, error) {
	return HarnessTurn{Available: false, Reason: d.reason}, nil
}

// stubBrain stands in for the API session.
type stubBrain struct {
	reply   string
	history models.AIChatHistory
}

func (s *stubBrain) SetTurnContext(string) {}
func (s *stubBrain) ProcessMessageWithheld(context.Context, string, []tools.Tool, map[string]bool) (string, []karmahelper.ToolCallRecord, error) {
	return s.reply, nil, nil
}
func (s *stubBrain) NeedsCompaction() bool             { return false }
func (s *stubBrain) GetHistory() *models.AIChatHistory { return &s.history }
func (s *stubBrain) SetHistory(h models.AIChatHistory) { s.history = h }
func (s *stubBrain) GetTotalTokens() int64             { return 0 }
func (s *stubBrain) GetKeepRecent() int                { return 0 }
func (s *stubBrain) ResetTokenCount()                  {}

// The whole point of the fallback: an engine that can be rate-limited needs one
// that cannot. A declined turn must be answered by the API path, not returned
// as an error — an error here is an agent that says nothing at all.
func TestADeclinedTurnFallsBackInsteadOfFailing(t *testing.T) {
	b := NewHarnessBrain(
		decliningSender{reason: "five_hour window is 89% used, past KARMAX's 40% share"},
		"agent:nexus", "agent", &stubBrain{reply: "answered by the API path"},
	)

	got, _, err := b.ProcessMessageWithheld(context.Background(), "are you there?", nil, nil)
	if err != nil {
		t.Fatalf("a declined turn returned an error instead of falling back: %v", err)
	}
	if got != "answered by the API path" {
		t.Errorf("the fallback did not answer; got %q", got)
	}
}

// Wired with no fallback — which is what the start-up ordering produced, since
// an agent's API session does not exist until agents start — a declined turn
// has nowhere to go. This documents the shape of that failure so the ordering
// cannot quietly regress into it.
func TestWithNoFallbackADeclinedTurnIsSilence(t *testing.T) {
	b := NewHarnessBrain(decliningSender{reason: "window exhausted"}, "agent:nexus", "agent", nil)

	got, _, err := b.ProcessMessageWithheld(context.Background(), "are you there?", nil, nil)
	if err == nil {
		t.Fatal("expected the failure a nil fallback causes")
	}
	if got != "" {
		t.Errorf("got a reply %q with no fallback wired", got)
	}
}

// A real transport error must also fall back, not surface.
func TestATransportErrorAlsoFallsBack(t *testing.T) {
	b := NewHarnessBrain(erroringSender{}, "agent:nexus", "agent", &stubBrain{reply: "still answered"})
	got, _, err := b.ProcessMessageWithheld(context.Background(), "hello", nil, nil)
	if err != nil {
		t.Fatalf("a transport error was not absorbed: %v", err)
	}
	if got != "still answered" {
		t.Errorf("got %q", got)
	}
}

type erroringSender struct{}

func (erroringSender) Send(context.Context, string, string, string) (HarnessTurn, error) {
	return HarnessTurn{}, errors.New("harness process died")
}
