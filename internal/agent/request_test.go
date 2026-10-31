package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
)

// fakeOwner is the task machinery: it records what it was handed.
type fakeOwner struct {
	mu       sync.Mutex
	handoffs []Handoff
	late     map[string][]HarnessTurn
	inflight []string
	err      error
}

func (o *fakeOwner) HandOff(_ context.Context, h Handoff) (HandoffResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return HandoffResult{}, o.err
	}
	o.handoffs = append(o.handoffs, h)
	return HandoffResult{TaskID: "req-" + h.Request.ID, Ack: "On it — I'll send the result here when it's done."}, nil
}

func (o *fakeOwner) LateResult(taskID string, t HarnessTurn) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.late == nil {
		o.late = map[string][]HarnessTurn{}
	}
	o.late[taskID] = append(o.late[taskID], t)
}

func (o *fakeOwner) InFlight(string) []string { return o.inflight }

// A request whose turn ran out of time is not handed to the fallback — a
// lighter, shorter model will not do in less time what this one could not —
// and the failure says the turn is unfinished, so the request can go on.
func TestATimedOutRequestIsUnfinishedNotRetriedOnTheFallback(t *testing.T) {
	fallback := &fakeBrain{reply: "answered by the fallback"}
	send := &fakeSender{turn: HarnessTurn{TimedOut: true}, err: errors.New("harness turn timed out after 3m0s")}
	b := NewHarnessBrain(send, "agent:nexus", "orchestrator", fallback)

	ctx := WithRequest(context.Background(), NewRequest("ev-1", RequestMessage))
	_, _, err := b.ProcessMessageWithheld(ctx, "make the onboarding deck for Northwind", nil, nil)
	if !errors.Is(err, ErrUnfinished) {
		t.Fatalf("err = %v, want it unfinished", err)
	}
	if !strings.Contains(err.Error(), "timed out after 3m0s") {
		t.Errorf("the error lost its cause: %v", err)
	}
	if fallback.called {
		t.Error("a timed-out request was run again on the fallback")
	}

	// A call that serves no request keeps the old behaviour.
	fallback.called = false
	if _, _, err := b.ProcessMessageWithheld(context.Background(), "summarise this", nil, nil); err != nil || !fallback.called {
		t.Errorf("a timed-out call with no request did not fall back: err=%v called=%v", err, fallback.called)
	}
}

// A turn that acted and then failed is unfinished too, whatever failed.
func TestATurnThatActedAndFailedIsUnfinished(t *testing.T) {
	send := &fakeSender{
		turn: HarnessTurn{ToolCalls: []HarnessToolCall{{Name: "Bash", Input: json.RawMessage(`{"command":"pandoc deck.md -o deck.pdf"}`)}}},
		err:  errors.New("harness exited mid-turn"),
	}
	b := NewHarnessBrain(send, "agent:nexus", "orchestrator", &fakeBrain{})
	_, calls, err := b.ProcessMessageWithheld(context.Background(), "go", nil, nil)
	if !errors.Is(err, ErrUnfinished) || len(calls) != 1 {
		t.Fatalf("err = %v, calls = %d; want unfinished with the call it made", err, len(calls))
	}
	if got := rootCause(err); got != "harness exited mid-turn" {
		t.Errorf("rootCause = %q, want the failure in its own words", got)
	}
}

// The rest of a turn a task took over belongs to that task — even when it
// arrives a moment before the hand-off is recorded.
func TestTheRestOfAHandedOffTurnGoesToItsTask(t *testing.T) {
	o := &fakeOwner{}
	a := replyTestAgent(t)
	a.SetTaskOwner(o)
	req := NewRequest("ev-2", RequestMessage)
	var toChat []HarnessTurn
	late := a.requestLate(req, func(t HarnessTurn) { toChat = append(toChat, t) })

	done := make(chan struct{})
	go func() {
		late(HarnessTurn{Text: "Wrote deck.pdf"})
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // the late turn is waiting for the decision
	req.setTaskID("req-ev-2")
	req.settle()
	<-done

	if len(toChat) != 0 {
		t.Errorf("the rest of a handed-off turn went to the chat: %+v", toChat)
	}
	if got := o.late["req-ev-2"]; len(got) != 1 || got[0].Text != "Wrote deck.pdf" {
		t.Errorf("the task got %+v", o.late)
	}

	// A turn nobody took over goes where its answer would have.
	req2 := NewRequest("ev-3", RequestMessage)
	req2.settle()
	a.requestLate(req2, func(t HarnessTurn) { toChat = append(toChat, t) })(HarnessTurn{Text: "late answer"})
	if len(toChat) != 1 {
		t.Errorf("the rest of a turn no task took over was lost: %+v", toChat)
	}
}

// The turn is told which request it serves, how to hand it over, and what
// is already being worked on for the same chat.
func TestTheTurnIsToldHowToHandALongRequestOver(t *testing.T) {
	o := &fakeOwner{inflight: []string{"Quarterly deck for Northwind (since 15:36)"}}
	a := replyTestAgent(t)
	a.SetTaskOwner(o)
	req := NewRequest("ev-4", RequestMessage)
	req.Target, req.Where = "chat-7", "the operator's own chat"

	got := a.requestContext(req)
	for _, want := range []string{"ev-4", "task.create request=ev-4", "Quarterly deck for Northwind", "do not answer them again"} {
		if !strings.Contains(got, want) {
			t.Errorf("the request context omits %q:\n%s", want, got)
		}
	}
	a.SetTaskOwner(nil)
	if got := a.requestContext(req); got != "" {
		t.Errorf("with nothing to take a request over, the turn was told to hand it over:\n%s", got)
	}
}

// A delegation's result names who was waiting for it, so the turn that
// reads it — long after the one that started it — knows where it goes.
func TestADelegationsResultSaysWhoIsWaiting(t *testing.T) {
	evt := bus.NewEvent(bus.EventDelegationDone, "nexus", map[string]any{
		"status": "completed", "task": "Build the Northwind deck", "output": "Wrote /tmp/deck.pdf",
		"origin_where": "the operator, in the chat \"Lakeside team\"", "origin_channel_id": "whatsapp-main",
		"origin_target": "120363000000000001@g.us",
	})
	p := buildPromptFromEvent(evt, nil)
	for _, want := range []string{"Lakeside team", "120363000000000001@g.us", "who is waiting", "Wrote /tmp/deck.pdf"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt omits %q:\n%s", want, p)
		}
	}
}

// With two requests in flight — the second waiting for the session behind
// the first — what a turn's tools are working for is the one the session
// took first; when it ends, the other.
func TestTheCurrentRequestIsTheOldestInFlight(t *testing.T) {
	a := replyTestAgent(t)
	first, second := NewRequest("ev-a", RequestMessage), NewRequest("ask-b", RequestAsk)
	endFirst := a.working(first)
	endSecond := a.working(second)
	if got := a.CurrentRequest(); got != first {
		t.Fatalf("current = %v, want the first", got)
	}
	if o := a.delegationOrigin(); o.RequestID != "ev-a" {
		t.Errorf("a delegation started now serves %q, want ev-a", o.RequestID)
	}
	endFirst()
	if got := a.CurrentRequest(); got != second {
		t.Fatalf("after the first ended, current = %v, want the second", got)
	}
	endSecond()
	if a.CurrentRequest() != nil {
		t.Error("a request is current after every turn ended")
	}
}

// A request survives the journal: what it was, where it came from.
func TestARequestRoundTripsThroughTheJournal(t *testing.T) {
	req := NewRequest("ask-1", RequestAsk)
	req.Text, req.ChannelID, req.Target, req.Where, req.Loop = "make the deck", "whatsapp-main", "group-1@g.us", "the group Lakeside", "chat-monitor"
	back, err := RequestFromJSON(req.JSON())
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != "ask-1" || back.Kind != RequestAsk || back.Text != "make the deck" || back.Target != "group-1@g.us" || back.Loop != "chat-monitor" {
		t.Errorf("round trip = %+v", back)
	}
	back.settle() // a read-back request can be settled like any other
}
