package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
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

	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "100000000000001@lid: ") || !strings.Contains((*sent)[0], "harness error") {
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

// The operator was told "something failed, please send it again" after a
// turn that had already acted — started the work, written to someone — so
// sending it again would have done it twice. The notice says what was already
// done, what failed, and not to send it again as it was.
func TestAFailedTurnThatActedSaysWhatWasDoneAndNotToResend(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"100000000000001@lid"})
	if _, err := a.store.UpsertContacts([]store.Contact{{Phone: "15555550123", Name: "Dana Reyes"}}); err != nil {
		t.Fatal(err)
	}
	sent := recordSends(a)
	started := time.Now().Add(-time.Minute).Truncate(time.Second)
	// What the turn sent to someone else before it failed.
	saveOutbound(t, a, "15555550123@s.whatsapp.net", "Starting the build now, will update you within the hour.")
	cause := &turnFailure{
		calls: []karmahelper.ToolCallRecord{
			{Name: "Bash", Input: map[string]any{"command": "fleetctl run agent-03 'build the export page'"}},
			{Name: "Read", Input: map[string]any{"file_path": "/srv/app/README.md"}},
			{Name: "Bash", Input: map[string]any{"command": "rm -rf /tmp/never-ran"}, Error: errors.New("denied")},
		},
		err: fmt.Errorf("main model: %w", fmt.Errorf("harness turn failed after 3 tool call(s): %w", errors.New("harness turn timed out after 3m0s"))),
	}

	a.tellOperatorTurnFailed(failedTurnEvent("100000000000001@lid"), started, cause)

	if len(*sent) != 1 {
		t.Fatalf("notices = %v, want one", *sent)
	}
	got := (*sent)[0]
	t.Logf("notice:\n%s", got)
	for _, want := range []string{
		"timed out after 3m0s",                 // what failed, in its own words
		"Dana Reyes", "Starting the build now", // what reached someone
		"fleetctl run agent-03", // what was started
		"don't send it again",   // whether to resend
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notice lacks %q:\n%s", want, got)
		}
	}
	for _, wrong := range []string{"Please send it again", "rm -rf /tmp/never-ran", "main model:"} {
		if strings.Contains(got, wrong) {
			t.Errorf("notice says %q:\n%s", wrong, got)
		}
	}
}

// A turn that failed before doing anything says so, and that it is safe to
// send again.
func TestAFailedTurnThatDidNothingSaysItIsSafeToResend(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"100000000000001@lid"})
	sent := recordSends(a)

	a.tellOperatorTurnFailed(failedTurnEvent("100000000000001@lid"), time.Now().Add(-time.Second),
		fmt.Errorf("main model: %w", errors.New("the harness is not available: the account's window is used up")))

	if len(*sent) != 1 {
		t.Fatalf("notices = %v, want one", *sent)
	}
	got := (*sent)[0]
	for _, want := range []string{"window is used up", "Nothing was done", "send it again"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice lacks %q:\n%s", want, got)
		}
	}
}

// A failed turn that acted is told whatever Jev thinks of the message: what
// was done as the operator is not something to leave unsaid.
func TestAFailedTurnThatActedIsToldEvenIfJevWouldNot(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"100000000000001@lid"})
	sent := recordSends(a)
	a.SetDecider(func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error) {
		return &loopkit.Decision{Answers: map[string]loopkit.Answer{"tell": {Noul: 0.1}}}, nil
	})
	cause := &turnFailure{calls: []karmahelper.ToolCallRecord{{Name: "Bash", Input: map[string]any{"command": "fleetctl run agent-03 x"}}},
		err: errors.New("harness exited mid-turn")}

	a.tellOperatorTurnFailed(failedTurnEvent("100000000000001@lid"), time.Now().Add(-time.Second), cause)

	if len(*sent) != 1 {
		t.Fatalf("a failed turn that acted was not reported: %v", *sent)
	}
}

// What a failed event turn sent as the operator is still reported, as a
// finished turn's would be: the failure does not swallow the "Sent as you".
func TestWhatAFailedEventTurnSentIsStillReported(t *testing.T) {
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{"100000000000001@lid"})
	var reports []string
	a.SetOperatorReport(func(text string) error { reports = append(reports, text); return nil })
	started := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := a.store.SaveChannelMessage(store.StoredChannelMessage{ID: "g1", ChannelID: "contoso/app#12",
		ChannelType: "github", SenderName: "octo-work", Direction: "outbound", Content: "approved"}); err != nil {
		t.Fatal(err)
	}

	a.afterFailedTurn(accountEvent(false), started, errors.New("harness turn timed out after 3m0s"))

	if len(reports) != 1 || !strings.Contains(reports[0], "GitHub as octo-work") {
		t.Errorf("reports = %v, want the review listed", reports)
	}
}

// The harness brain hands back what a failed turn did, so the agent can say it.
func TestAFailedHarnessTurnReturnsWhatItDid(t *testing.T) {
	send := &fakeSender{
		turn: HarnessTurn{ToolCalls: []HarnessToolCall{{Name: "Bash", Input: json.RawMessage(`{"command":"fleetctl run agent-03"}`)}}},
		err:  errors.New("harness turn timed out after 3m0s"),
	}
	b := NewHarnessBrain(send, "agent:nexus", "orchestrator", &countingBrain{})
	_, calls, err := b.ProcessMessageWithheld(context.Background(), "go ahead", nil, nil)
	if err == nil {
		t.Fatal("a failed turn came back as a success")
	}
	if len(calls) != 1 || calls[0].Input["command"] != "fleetctl run agent-03" {
		t.Errorf("calls = %+v, want the command it ran", calls)
	}
}
