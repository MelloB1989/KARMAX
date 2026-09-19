package reflex

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karma/ai/jev"
	"github.com/MelloB1989/karmax/internal/bus"
)

// The sheet is refused client-side if a question could not produce a usable
// answer, and that refusal happens before any request — so pointing at an
// unroutable address still exercises it.
func TestSheetIsValid(t *testing.T) {
	c, err := jev.New(jev.WithAPIKey("test-key"),
		jev.WithBaseURL("http://127.0.0.1:1"),
		jev.WithTimeout(time.Second),
		jev.WithRetry(jev.RetryPolicy{}))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	_, err = c.Evaluate(context.Background(), map[string]any{"kind": "test"}, Sheet())
	if err == nil {
		t.Fatal("expected a transport error, got none")
	}
	var ve *jev.ValidationError
	if errors.As(err, &ve) {
		t.Fatalf("the sheet is not a valid request: %v", err)
	}
}

// Every disposition option must map to an Action the policy accepts, or a
// perfectly good answer would fail open.
func TestSheetDispositionsAreKnownActions(t *testing.T) {
	q, ok := Sheet()[QDisposition].(jev.ChoiceQuestion)
	if !ok {
		t.Fatal("disposition is not a choice question")
	}
	if len(q.Criteria) == 0 {
		t.Fatal("disposition has no options")
	}
	for opt := range q.Criteria {
		if !validAction(Action(opt)) {
			t.Errorf("option %q is not a valid action", opt)
		}
	}
}

func TestSheetEffortsAreKnown(t *testing.T) {
	q := Sheet()[QEffort].(jev.ChoiceQuestion)
	for opt := range q.Criteria {
		if !validEffort(Effort(opt)) {
			t.Errorf("option %q is not a valid effort", opt)
		}
	}
}

// The rubric length and the normalise constant must not drift apart, or every
// urgency and risk is silently mis-scaled.
func TestRubricLengthsMatchConstants(t *testing.T) {
	s := Sheet()
	if got := len(s[QUrgency].(jev.ScoreQuestion).Criteria); got != urgencyLevels {
		t.Errorf("urgency rubric has %d levels, urgencyLevels is %d", got, urgencyLevels)
	}
	if got := len(s[QRisk].(jev.ScoreQuestion).Criteria); got != riskLevels {
		t.Errorf("risk rubric has %d levels, riskLevels is %d", got, riskLevels)
	}
}

func TestStateOfMarksTheOperator(t *testing.T) {
	evt := bus.NewEvent(bus.EventCommsMessage, "agent", map[string]any{"content": "ping"})
	state := StateOf(evt, Hint{Operator: true, Sender: "Someone Else"})
	from, _ := state["from"].(string)
	if !strings.Contains(from, "operator") {
		t.Fatalf("from = %q, want the operator marking to win over the sender name", from)
	}
}

func TestStateOfDropsInternalRouting(t *testing.T) {
	evt := bus.NewEvent(bus.EventCommsMessage, "agent", map[string]any{
		"content":           "hello",
		"karmax_channel_id": "wa-1",
		"empty":             "",
	})
	payload := StateOf(evt, Hint{})["payload"].(map[string]any)
	if _, ok := payload["karmax_channel_id"]; ok {
		t.Error("internal routing ids must not be spent on tokens")
	}
	if _, ok := payload["empty"]; ok {
		t.Error("empty values must be omitted")
	}
	if payload["content"] != "hello" {
		t.Errorf("content = %v, want it kept", payload["content"])
	}
}

func TestStateOfClipsLongFields(t *testing.T) {
	evt := bus.NewEvent(bus.EventCommsMessage, "agent", map[string]any{
		"content": strings.Repeat("x", maxFieldRunes*3),
	})
	payload := StateOf(evt, Hint{})["payload"].(map[string]any)
	if got := len([]rune(payload["content"].(string))); got > maxFieldRunes+1 {
		t.Errorf("content kept %d runes, want it clipped to %d", got, maxFieldRunes)
	}
}

// A nil evaluator is the no-key path, and it must behave like reflex was never
// added rather than panic on the hot path.
func TestNilEvaluatorFailsOpen(t *testing.T) {
	var e *Evaluator
	v := e.Screen(context.Background(), bus.NewEvent(bus.EventCommsMessage, "a", nil), Hint{})
	if v.Action != ActionHandle || !v.FailedOpen {
		t.Fatalf("nil evaluator should fail open, got %+v", v)
	}
	if e.Available() {
		t.Error("a nil evaluator is not available")
	}
	if e.Stats().Screened != 0 {
		t.Error("a nil evaluator has no stats")
	}
}
