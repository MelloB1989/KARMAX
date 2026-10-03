package agent

import (
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
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
	a.SetOperatorChats([]string{"5794649083972@lid"})
	sent := recordSends(a)

	a.tellOperatorTurnFailed(failedTurnEvent("5794649083972@lid"), time.Now().Add(-time.Second))

	if len(*sent) != 1 || (*sent)[0] != "5794649083972@lid: "+turnFailedNotice {
		t.Fatalf("operator was not told the turn failed: %v", *sent)
	}
}

// A stranger never hears about KARMAX's failures.
func TestAFailedMonitoredTurnStaysQuiet(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"5794649083972@lid"})
	sent := recordSends(a)

	a.tellOperatorTurnFailed(failedTurnEvent("17671837092@s.whatsapp.net"), time.Now().Add(-time.Second))

	if len(*sent) != 0 {
		t.Fatalf("a monitored chat was told about a failure: %v", *sent)
	}
}

// A turn that answered before failing has said its piece.
func TestAFailedTurnThatAlreadyRepliedSendsNothingMore(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"5794649083972@lid"})
	sent := recordSends(a)
	started := time.Now().Add(-time.Minute).Truncate(time.Second)
	saveOutbound(t, a, "5794649083972@lid", "Yes, it happened at 10.")

	a.tellOperatorTurnFailed(failedTurnEvent("5794649083972@lid"), started)

	if len(*sent) != 0 {
		t.Fatalf("a turn that replied was followed by a failure notice: %v", *sent)
	}
}
