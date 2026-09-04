package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
)

type fakeSender struct {
	turn HarnessTurn
	err  error
	got  string
	n    int
}

func (f *fakeSender) Send(_ context.Context, _, _, text string) (HarnessTurn, error) {
	f.n++
	f.got = text
	return f.turn, f.err
}

type fakeBrain struct {
	called bool
	reply  string
}

func (f *fakeBrain) ProcessMessageWithheld(_ context.Context, msg string, _ []tools.Tool, _ map[string]bool) (string, []karmahelper.ToolCallRecord, error) {
	f.called = true
	return f.reply, nil, nil
}
func (f *fakeBrain) SetTurnContext(string)             {}
func (f *fakeBrain) NeedsCompaction() bool             { return false }
func (f *fakeBrain) GetHistory() *models.AIChatHistory { return &models.AIChatHistory{} }
func (f *fakeBrain) SetHistory(models.AIChatHistory)   {}
func (f *fakeBrain) GetTotalTokens() int64             { return 0 }
func (f *fakeBrain) GetKeepRecent() int                { return 10 }
func (f *fakeBrain) ResetTokenCount()                  {}

// The account's windows are shared with the operator and run out routinely, so
// a refused turn has to be an ordinary handover, not an error a person sees.
func TestARefusedTurnFallsBackSilently(t *testing.T) {
	send := &fakeSender{turn: HarnessTurn{Available: false, Reason: "seven_day window is 88% used"}}
	fb := &fakeBrain{reply: "answered by the API path"}
	b := NewHarnessBrain(send, "k", "agent", fb)

	got, _, err := b.ProcessMessageWithheld(context.Background(), "hello", nil, nil)
	if err != nil {
		t.Fatalf("a tripped breaker must not surface as an error: %v", err)
	}
	if !fb.called {
		t.Error("the fallback brain was never used")
	}
	if got != "answered by the API path" {
		t.Errorf("reply = %q", got)
	}
}

// Half of KARMAX decides from the tool-call list: the act-evidence guard, the
// recent-actions context, the duplicate-send counters. A brain that dropped it
// would make a working agent look like one that promises and does nothing.
func TestToolCallsSurviveTheCrossing(t *testing.T) {
	send := &fakeSender{turn: HarnessTurn{
		Available: true, Text: "done",
		ToolCalls: []HarnessToolCall{{Name: "comms_send", Input: json.RawMessage(`{"to":"x","text":"hi"}`)}},
	}}
	b := NewHarnessBrain(send, "k", "agent", nil)

	_, records, err := b.ProcessMessageWithheld(context.Background(), "send it", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Name != "comms_send" {
		t.Fatalf("records = %+v", records)
	}
	if records[0].Input["to"] != "x" {
		t.Errorf("tool input did not survive: %+v", records[0].Input)
	}
}

// Per-turn context goes first and the operator's message last, the same order
// the API path uses — the model should meet the same material the same way
// whichever engine is running.
func TestPromptPutsContextFirstAndMessageLast(t *testing.T) {
	send := &fakeSender{turn: HarnessTurn{Available: true, Text: "ok"}}
	b := NewHarnessBrain(send, "k", "agent", nil)
	b.SetTurnContext("## Current time\nIt is 9pm.")

	if _, _, err := b.ProcessMessageWithheld(context.Background(), "what is next?", nil, nil); err != nil {
		t.Fatal(err)
	}
	ctxAt := strings.Index(send.got, "Current time")
	msgAt := strings.Index(send.got, "what is next?")
	if ctxAt < 0 || msgAt < 0 {
		t.Fatalf("prompt lost a part: %q", send.got)
	}
	if ctxAt > msgAt {
		t.Error("context must precede the message")
	}
}

// The context is consumed by the turn it was set for; a later turn must not
// silently reuse a stale clock.
func TestContextIsNotReusedOnTheNextTurn(t *testing.T) {
	send := &fakeSender{turn: HarnessTurn{Available: true, Text: "ok"}}
	b := NewHarnessBrain(send, "k", "agent", nil)
	b.SetTurnContext("FIRST-TURN-CONTEXT")

	_, _, _ = b.ProcessMessageWithheld(context.Background(), "one", nil, nil)
	_, _, _ = b.ProcessMessageWithheld(context.Background(), "two", nil, nil)

	if strings.Contains(send.got, "FIRST-TURN-CONTEXT") {
		t.Error("the second turn reused the first turn's context")
	}
}

// A withheld pass must be told, since the harness owns its own toolset and a
// tool cannot simply be removed from it.
func TestWithheldToolsAreStatedInThePrompt(t *testing.T) {
	send := &fakeSender{turn: HarnessTurn{Available: true, Text: "ok"}}
	b := NewHarnessBrain(send, "k", "agent", nil)

	_, _, _ = b.ProcessMessageWithheld(context.Background(), "observe only", nil, map[string]bool{"comms_send": true})
	if !strings.Contains(send.got, "comms_send") || !strings.Contains(send.got, "WITHHELD") {
		t.Errorf("the withhold was not stated: %q", send.got)
	}
}

// The harness keeps its own transcript. Reporting compaction would start a
// second summarisation of a conversation already being managed.
func TestTheHarnessOwnsItsTranscript(t *testing.T) {
	b := NewHarnessBrain(&fakeSender{}, "k", "agent", nil)
	if b.NeedsCompaction() {
		t.Error("the harness manages its own history; KARMAX must not compact it too")
	}
}
