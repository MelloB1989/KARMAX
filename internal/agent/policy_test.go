package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/policy"
	"github.com/MelloB1989/karmax/internal/sources"
	"github.com/MelloB1989/karmax/internal/store"
)

const testOperatorChat = "100000000000001@lid"

func policyAgent(t *testing.T, p policy.Policy) *Agent {
	t.Helper()
	a := replyTestAgent(t)
	a.SetOperatorChats([]string{testOperatorChat})
	a.SetPolicy(p)
	return a
}

func outbound(t *testing.T, a *Agent, id, target, producer, content string) {
	t.Helper()
	if err := a.store.SaveChannelMessage(store.StoredChannelMessage{
		ID: id, ChannelID: target, ChannelType: "whatsapp", SenderID: producer,
		Direction: "outbound", Content: content,
	}); err != nil {
		t.Fatal(err)
	}
}

func accountEvent(urgent bool) bus.Event {
	return sources.Event{Source: "github", Account: "octo-work", Type: "review_requested",
		Title: "Review requested on acme/api#12", Summary: "Dana wants a review", At: time.Now(),
		Urgent: urgent, Data: map[string]any{"repo": "acme/api"}}.BusEvent("nexus")
}

// The model is told the operating policy every turn, so it acts on it rather
// than on whatever older instruction it remembers.
func TestThePolicyReachesEveryTurn(t *testing.T) {
	a := policyAgent(t, policy.New(policy.Config{HardFloor: []string{"paying invoices"}}))
	got := a.buildPolicyContext()
	if !strings.Contains(got, "Act, then report") || !strings.Contains(got, "paying invoices") {
		t.Errorf("policy context = %q", got)
	}
	// An agent nobody configured still runs on the operator's decided default.
	if !strings.Contains((&Agent{}).buildPolicyContext(), "Act, then report") {
		t.Error("an unconfigured agent has no policy")
	}
}

// Under act-then-report the screener's risk flag means "be sure", not "ask",
// and an escalation means "deal with it now; the operator will be told".
func TestTriageFollowsThePolicy(t *testing.T) {
	act := policy.Default()
	approval := screeningOf(screenedEvent(map[string]string{
		bus.MetaReflexAction: "handle", bus.MetaReflexApproval: "required"})).contextFor(act)
	if strings.Contains(approval, "Propose it for approval") || !strings.Contains(approval, "hard floor") {
		t.Errorf("act-first approval triage = %q", approval)
	}
	escalate := screeningOf(screenedEvent(map[string]string{bus.MetaReflexAction: "escalate"})).contextFor(act)
	if strings.Contains(escalate, "ask them directly") || !strings.Contains(escalate, "time-critical") {
		t.Errorf("act-first escalate triage = %q", escalate)
	}
	ask := policy.New(policy.Config{Autonomy: "ask"})
	if got := screeningOf(screenedEvent(map[string]string{
		bus.MetaReflexAction: "handle", bus.MetaReflexApproval: "required"})).contextFor(ask); !strings.Contains(got, "propose") {
		t.Errorf("ask-first approval triage = %q", got)
	}
}

// What KARMAX sent as the operator during a turn is told to them in one
// message: comms sends from the store, and sends through other tools as the
// model declared them.
func TestATurnReportsWhatWasSentAsTheOperator(t *testing.T) {
	a := policyAgent(t, policy.Default())
	if _, err := a.store.UpsertContacts([]store.Contact{{Phone: "15555550123", Name: "Dana Reyes"}}); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Minute)
	outbound(t, a, "m1", "15555550123@s.whatsapp.net", "", "Reviewing it now, will approve within the hour")
	outbound(t, a, "m2", testOperatorChat, "", "a reply to the operator")
	outbound(t, a, "m3", "15555550199@s.whatsapp.net", "loop", "a loop's post")
	outbound(t, a, "m4", "15555550188@s.whatsapp.net", "", ackMessage)
	// What the GitHub acting tools record: done as one of the operator's logins.
	if err := a.store.SaveChannelMessage(store.StoredChannelMessage{ID: "g1", ChannelID: "contoso/app#12",
		ChannelType: "github", SenderName: "octo-work", Direction: "outbound", Content: "commented on: LGTM"}); err != nil {
		t.Fatal(err)
	}

	report := a.turnReport(accountEvent(false), since,
		"Approved the fix and told Dana.\nSENT: github/octo-work → acme/api#12: approving review")
	for _, want := range []string{"Sent as you", "Dana Reyes", "Reviewing it now", "github/octo-work → acme/api#12: approving review",
		"GitHub as octo-work → contoso/app#12: “commented on: LGTM”"} {
		if !strings.Contains(report, want) {
			t.Errorf("report omits %q:\n%s", want, report)
		}
	}
	for _, leak := range []string{"a reply to the operator", "a loop's post", ackMessage, "Urgent"} {
		if strings.Contains(report, leak) {
			t.Errorf("report includes %q:\n%s", leak, report)
		}
	}
	// Reported once: the next turn does not repeat it.
	if again := a.turnReport(accountEvent(false), since, "nothing more"); again != "" {
		t.Errorf("a send was reported twice:\n%s", again)
	}
}

// An urgent event reaches the operator with what was done about it.
func TestAnUrgentTurnIsReported(t *testing.T) {
	a := policyAgent(t, policy.Default())
	report := a.turnReport(accountEvent(true), time.Now(), "Looked at the PR; it blocks the release, so I asked Dana for an ETA.")
	for _, want := range []string{"Urgent", "Review requested on acme/api#12", "github", "octo-work", "asked Dana for an ETA"} {
		if !strings.Contains(report, want) {
			t.Errorf("report omits %q:\n%s", want, report)
		}
	}
	// Screening urgency at the threshold counts too.
	evt := accountEvent(false)
	evt.Meta[bus.MetaReflexAction] = "escalate"
	evt.Meta[bus.MetaReflexUrgency] = "0.95"
	if !strings.Contains(a.turnReport(evt, time.Now(), "Handled."), "Urgent") {
		t.Error("an event screened at the urgency threshold was not reported as urgent")
	}
}

// A source's first look at an account brings a day of backlog, urgent items
// included; at deploy that paged the operator once per item. Backlog, and
// anything that happened hours before it arrived, is never a page — even when
// the screener scored it at the threshold.
func TestBacklogAndStaleItemsNeverPage(t *testing.T) {
	a := policyAgent(t, policy.Default())
	backlog := sources.Event{Source: "github", Account: "octo-work", Title: "Review requested on acme/api#12",
		At: time.Now().Add(-20 * time.Minute), Urgent: true, Backlog: true}.BusEvent("nexus")
	backlog.Meta[bus.MetaReflexUrgency] = "0.95"
	if got := a.turnReport(backlog, time.Now(), "Checked it; already reviewed."); got != "" {
		t.Errorf("a backlog item paged the operator:\n%s", got)
	}
	stale := sources.Event{Source: "aws", Account: "prod", Title: "api-5xx in ALARM",
		At: time.Now().Add(-5 * time.Hour), Urgent: true}.BusEvent("nexus")
	if got := a.turnReport(stale, time.Now(), "It recovered an hour later."); got != "" {
		t.Errorf("a five-hour-old item paged the operator:\n%s", got)
	}
}

// Routine work is not reported; the operator's own conversation needs no
// report (they are in it); and a policy that keeps WhatsApp quiet is obeyed.
func TestRoutineAndConversationalTurnsAreNotReported(t *testing.T) {
	a := policyAgent(t, policy.Default())
	if got := a.turnReport(accountEvent(false), time.Now(), "Filed it."); got != "" {
		t.Errorf("a routine turn was reported:\n%s", got)
	}
	outbound(t, a, "m5", "15555550123@s.whatsapp.net", "", "Sure, booked")
	op := bus.NewEvent(bus.EventCommsMessage, "nexus", map[string]any{"channel_id": testOperatorChat, "content": "tell Dana yes"})
	if got := a.turnReport(op, time.Now().Add(-time.Minute), "Told Dana."); got != "" {
		t.Errorf("the operator's own conversation was reported back to them:\n%s", got)
	}

	quiet := policyAgent(t, policy.New(policy.Config{WhatsApp: []string{"approvals"}}))
	outbound(t, quiet, "m6", "15555550123@s.whatsapp.net", "", "Sure, booked")
	if got := quiet.turnReport(accountEvent(true), time.Now().Add(-time.Minute), "Done."); got != "" {
		t.Errorf("reported against the policy:\n%s", got)
	}
}

// The report goes to the operator's WhatsApp through the injected sender.
func TestTheReportIsDelivered(t *testing.T) {
	a := policyAgent(t, policy.Default())
	var got []string
	a.SetOperatorReport(func(text string) error { got = append(got, text); return nil })
	a.deliverTurnReport(accountEvent(true), time.Now(), "Checked it.")
	if len(got) != 1 || !strings.Contains(got[0], "Urgent") {
		t.Errorf("delivered %v", got)
	}
}

// comms.escalate is a second way to ask permission; under act-then-report it
// follows the same floor rule as propose.
func TestEscalateFollowsTheFloor(t *testing.T) {
	a := policyAgent(t, policy.Default())
	var asked []string
	a.SetCommsEscalate(func(_, _, content string) error { asked = append(asked, content); return nil })
	tool := &commsEscalateTool{agent: a}

	res, _ := tool.Execute(context.Background(), map[string]any{"action": "reply to Dana", "risk": "it is a message"})
	if res.IsError || len(asked) != 0 {
		t.Fatalf("an off-floor escalation went out: %v %+v", asked, res)
	}
	if out, _ := res.Output.(map[string]any); out["status"] != "not_needed" {
		t.Errorf("result = %+v", res.Output)
	}
	res, _ = tool.Execute(context.Background(), map[string]any{"action": "delete the staging bucket", "risk": "irreversible",
		"floor": "deleting repositories, data or cloud resources"})
	if res.IsError || len(asked) != 1 {
		t.Fatalf("an on-floor escalation did not go out: %v %+v", asked, res)
	}
}
