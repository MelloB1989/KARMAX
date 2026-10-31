package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/policy"
	"github.com/MelloB1989/karmax/internal/store"
)

// A turn's harness call carries where the rest of the turn goes if it ends
// after the call gave up.
func TestALateReplyHandlerTravelsWithTheCall(t *testing.T) {
	if LateReplyFrom(context.Background()) != nil {
		t.Fatal("a bare context carries a late-reply handler")
	}
	var got []HarnessTurn
	ctx := WithLateReply(context.Background(), func(t HarnessTurn) { got = append(got, t) })
	fn := LateReplyFrom(ctx)
	if fn == nil {
		t.Fatal("the handler did not travel with the context")
	}
	fn(HarnessTurn{Text: "done"})
	if len(got) != 1 || got[0].Text != "done" {
		t.Errorf("handler got %+v", got)
	}
}

// The operator's message timed out; the session finished it anyway. Its
// answer reaches the operator's chat instead of vanishing.
func TestALateReplyReachesTheOperatorsChat(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"100000000000001@lid"})
	sent := recordSends(a)
	started := time.Now().Add(-5 * time.Minute).Truncate(time.Second)

	a.deliverLateReply(failedTurnEvent("100000000000001@lid"), started, "did the standup happen?",
		HarnessTurn{Available: true, Text: "Yes — it happened at 10, notes are in the doc."}, false)

	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "100000000000001@lid: ") ||
		!strings.Contains((*sent)[0], "it happened at 10") {
		t.Fatalf("the late answer did not reach the chat: %v", *sent)
	}
}

// A late reply is never a second answer: not when the turn was answered
// anyway (the fallback took it), and not when the chat was answered since.
func TestALateReplyIsNotASecondAnswer(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"100000000000001@lid"})
	sent := recordSends(a)
	started := time.Now().Add(-5 * time.Minute).Truncate(time.Second)

	a.deliverLateReply(failedTurnEvent("100000000000001@lid"), started, "q", HarnessTurn{Text: "late words"}, true)
	if len(*sent) != 0 {
		t.Fatalf("a turn already answered was answered again: %v", *sent)
	}
	saveOutbound(t, a, "100000000000001@lid", "Yes, at 10.")
	a.deliverLateReply(failedTurnEvent("100000000000001@lid"), started, "q", HarnessTurn{Text: "late words"}, false)
	if len(*sent) != 0 {
		t.Fatalf("a chat answered since was answered again: %v", *sent)
	}
}

// The rest of an urgent event's turn is reported to the operator per policy:
// it finished, and what it did. A routine event's is not.
func TestALateReplyToAnUrgentEventIsReported(t *testing.T) {
	a := policyAgent(t, policy.Default())
	var reports []string
	a.SetOperatorReport(func(text string) error { reports = append(reports, text); return nil })
	started := time.Now().Add(-5 * time.Minute).Truncate(time.Second)

	a.deliverLateReply(accountEvent(false), started, "p", HarnessTurn{Text: "Reviewed it."}, false)
	if len(reports) != 0 {
		t.Fatalf("a routine event's late reply paged the operator: %v", reports)
	}
	a.deliverLateReply(accountEvent(true), started, "p", HarnessTurn{Text: "Approved the fix after CI went green."}, false)
	if len(reports) != 1 || !strings.Contains(reports[0], "Approved the fix") || !strings.Contains(reports[0], "Review requested on acme/api#12") {
		t.Fatalf("reports = %v, want the urgent event's late result", reports)
	}
	if strings.Contains(reports[0], "⚠️ Urgent") {
		t.Errorf("the late result repeated the urgent page instead of following it up:\n%s", reports[0])
	}
}

// A turn the agent's session ran with no conversation waiting — a peer's
// message it answered — is kept in the app, and what it sent as the operator
// is reported like any turn's.
func TestABackgroundTurnIsReportedPerPolicy(t *testing.T) {
	a := policyAgent(t, policy.Default())
	var reports []string
	a.SetOperatorReport(func(text string) error { reports = append(reports, text); return nil })
	since := time.Now().Add(-time.Minute)
	if err := a.store.SaveChannelMessage(store.StoredChannelMessage{ID: "b1", ChannelID: "contoso/app#12",
		ChannelType: "github", SenderName: "octo-work", Direction: "outbound", Content: "merged"}); err != nil {
		t.Fatal(err)
	}

	a.ReportBackgroundTurn(since, "agent-03 finished the fix; I merged it.")
	a.ReportBackgroundTurn(time.Now(), "agent-05 is blocked on a failing migration.")

	notes, _ := a.store.ListNotifications(10)
	found := map[string]bool{}
	for _, n := range notes {
		for _, w := range []string{"agent-03 finished the fix", "agent-05 is blocked"} {
			if strings.Contains(n.Body, w) {
				found[w] = true
			}
		}
	}
	if len(found) != 2 {
		t.Errorf("each background turn's words should be in the app, got %v from %+v", found, notes)
	}
	if len(reports) != 1 || !strings.Contains(reports[0], "GitHub as octo-work") {
		t.Errorf("reports = %v, want what it sent as the operator", reports)
	}
}
