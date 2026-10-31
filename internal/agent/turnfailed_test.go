package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

func failedTurnEvent(chat string) bus.Event {
	return bus.NewEvent(bus.EventCommsMessage, "whatsapp", map[string]any{
		"karmax_channel_id": "wa", "channel_id": chat, "content": "did the standup happen?",
	})
}

func recordSends(a *Agent) *[]string {
	var sent []string
	a.commsSend = func(_, target, content string) error {
		sent = append(sent, target+": "+content)
		return nil
	}
	return &sent
}

// The operator's DM whose turn failed got nothing back, which reads as being
// ignored. Now it gets told.
func TestAFailedOperatorTurnIsAnswered(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"100000000000001@lid"})
	sent := recordSends(a)

	a.tellOperatorTurnFailed(failedTurnEvent("100000000000001@lid"), time.Now().Add(-time.Second), errors.New("harness error"))

	if len(*sent) != 1 || (*sent)[0] != "100000000000001@lid: "+turnFailedNotice {
		t.Fatalf("operator was not told the turn failed: %v", *sent)
	}
}

// A stranger never hears about KARMAX's failures.
func TestAFailedMonitoredTurnStaysQuiet(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"100000000000001@lid"})
	sent := recordSends(a)

	a.tellOperatorTurnFailed(failedTurnEvent("15555550102@s.whatsapp.net"), time.Now().Add(-time.Second), errors.New("harness error"))

	if len(*sent) != 0 {
		t.Fatalf("a monitored chat was told about a failure: %v", *sent)
	}
}

// A turn that answered before failing has said its piece.
func TestAFailedTurnThatAlreadyRepliedSendsNothingMore(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"100000000000001@lid"})
	sent := recordSends(a)
	started := time.Now().Add(-time.Minute).Truncate(time.Second)
	saveOutbound(t, a, "100000000000001@lid", "Yes, it happened at 10.")

	a.tellOperatorTurnFailed(failedTurnEvent("100000000000001@lid"), started, errors.New("harness error"))

	if len(*sent) != 0 {
		t.Fatalf("a turn that replied was followed by a failure notice: %v", *sent)
	}
}

// The notice is the same words every time, so it goes out as a turn status:
// sent as an ordinary message, the duplicate guard refused a second failure's
// notice for six hours after the first, and that failure was silence.
func TestTheFailureNoticeGoesOutAsATurnStatus(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"100000000000001@lid"})
	ordinary := recordSends(a)
	var status []string
	a.SetStatusSend(func(_, target, content string) error {
		status = append(status, target+": "+content)
		return nil
	})

	for range 2 {
		a.tellOperatorTurnFailed(failedTurnEvent("100000000000001@lid"), time.Now().Add(-time.Second), errors.New("harness error"))
	}

	if len(status) != 2 {
		t.Errorf("status notices = %v, want one per failed turn", status)
	}
	if len(*ordinary) != 0 {
		t.Errorf("the notice went out as an ordinary message: %v", *ordinary)
	}
}

// Jev decides whether a failure is worth telling; a no keeps the chat quiet.
func TestJevCanDecideAFailedTurnNeedsNoNotice(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"100000000000001@lid"})
	sent := recordSends(a)
	a.SetDecider(func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error) {
		return &loopkit.Decision{Answers: map[string]loopkit.Answer{"tell": {Noul: 0.1}}}, nil
	})

	a.tellOperatorTurnFailed(failedTurnEvent("100000000000001@lid"), time.Now().Add(-time.Second), errors.New("harness error"))

	if len(*sent) != 0 {
		t.Fatalf("sent despite Jev saying no: %v", *sent)
	}
}
