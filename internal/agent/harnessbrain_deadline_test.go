package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
)

// countingBrain is a fallback that records whether it was asked.
type countingBrain struct {
	calls  int
	budget time.Duration
}

func (c *countingBrain) ProcessMessageWithheld(context.Context, string, []tools.Tool, map[string]bool) (string, []karmahelper.ToolCallRecord, error) {
	c.calls++
	return "answered by the fallback", nil, nil
}
func (c *countingBrain) SetTurnContext(string)             {}
func (c *countingBrain) NeedsCompaction() bool             { return false }
func (c *countingBrain) GetHistory() *models.AIChatHistory { return &models.AIChatHistory{} }
func (c *countingBrain) SetHistory(models.AIChatHistory)   {}
func (c *countingBrain) GetTotalTokens() int64             { return 0 }
func (c *countingBrain) GetKeepRecent() int                { return 10 }
func (c *countingBrain) ResetTokenCount()                  {}
func (c *countingBrain) TurnBudget() time.Duration         { return c.budget }

// The operator said "go ahead and build it". The orchestrator's session ran
// the commands that started the work, then the turn was cut off at the
// deadline — and the same message went to the API brain, which knows nothing
// of what had just been started. A turn that already acted is not re-run
// somewhere else: doing it twice is worse than reporting it failed.
func TestAHarnessTurnThatActedIsNotRunAgainOnTheFallback(t *testing.T) {
	send := &fakeSender{
		turn: HarnessTurn{ToolCalls: []HarnessToolCall{{Name: "Bash", Input: json.RawMessage(`{"command":"fleetctl run"}`)}}},
		err:  errors.New("harness turn timed out after 3m0s"),
	}
	fb := &countingBrain{}
	b := NewHarnessBrain(send, "agent:nexus", "orchestrator", fb)

	_, _, err := b.ProcessMessageWithheld(context.Background(), "go ahead", nil, nil)
	if err == nil {
		t.Fatal("a turn that acted and then failed came back as a success")
	}
	if fb.calls != 0 {
		t.Error("the fallback was asked to redo a turn the harness had already acted on")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q lost the harness's own reason", err)
	}
}

// With the turn's time already spent, the fallback cannot answer: it only
// spawned a process that failed in the same millisecond, and the error the
// operator's turn recorded was the fallback's "context deadline exceeded"
// instead of what actually went wrong.
func TestNoFallbackOnceTheTurnsTimeIsGone(t *testing.T) {
	send := &fakeSender{err: errors.New("harness: the session is busy with a turn another session asked for")}
	fb := &countingBrain{}
	b := NewHarnessBrain(send, "agent:nexus", "orchestrator", fb)

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-ctx.Done()

	_, _, err := b.ProcessMessageWithheld(ctx, "hello", nil, nil)
	if err == nil {
		t.Fatal("expected an error with no time left")
	}
	if fb.calls != 0 {
		t.Error("the fallback was called with a context that had already expired")
	}
	if !strings.Contains(err.Error(), "busy") {
		t.Errorf("error %q hides the harness's reason behind the deadline", err)
	}
}

// A turn that never reached the harness still falls back: that is what the
// fallback is for, and nothing can have happened twice.
func TestAHarnessThatNeverTookTheTurnStillFallsBack(t *testing.T) {
	send := &fakeSender{err: errors.New("harness: the session is busy with a turn another session asked for")}
	fb := &countingBrain{}
	b := NewHarnessBrain(send, "agent:nexus", "orchestrator", fb)

	got, _, err := b.ProcessMessageWithheld(context.Background(), "hello", nil, nil)
	if err != nil || got != "answered by the fallback" || fb.calls != 1 {
		t.Fatalf("got %q, %v after %d fallback calls; want the fallback's answer", got, err, fb.calls)
	}
}

// budgetSender is a harness that says how long its turns may take.
type budgetSender struct {
	fakeSender
	budget map[string]time.Duration
}

func (b *budgetSender) TurnBudget(kind string) time.Duration { return b.budget[kind] }

// The brain's budget is the harness's, for its kind, plus the fallback's own:
// a fallback that runs on whatever the harness left over has nothing left.
func TestTheHarnessBrainsBudgetCoversTheHarnessAndTheFallback(t *testing.T) {
	send := &budgetSender{budget: map[string]time.Duration{"orchestrator": 7 * time.Minute}}
	b := NewHarnessBrain(send, "agent:nexus", "orchestrator", &countingBrain{budget: 4 * time.Minute})

	if got := turnBudget(b); got != 11*time.Minute {
		t.Errorf("turnBudget = %s, want the harness's 7m plus the fallback's 4m", got)
	}
	// A brain that says nothing keeps the old bound, and none is ever shorter.
	if got := turnBudget(&countingBrain{}); got != defaultTurnBudget {
		t.Errorf("turnBudget of a brain with no budget = %s, want %s", got, defaultTurnBudget)
	}
	if got := turnBudget(&countingBrain{budget: time.Second}); got != defaultTurnBudget {
		t.Errorf("turnBudget = %s; a brain may lengthen its turn, never shorten it below %s", got, defaultTurnBudget)
	}
}
