package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/api"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/policy"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

type sendSink struct {
	mu   sync.Mutex
	sent []string
}

func (s *sendSink) send(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, text)
	return nil
}

func (s *sendSink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// jevAnswers is a Jev that answers every question it is asked from a table,
// and counts how often it was asked.
func jevAnswers(asked *int, answers map[string]loopkit.Answer) func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error) {
	return func(_ context.Context, _ any, qs loopkit.Questions) (*loopkit.Decision, error) {
		if asked != nil {
			*asked++
		}
		d := &loopkit.Decision{Answers: map[string]loopkit.Answer{}}
		for id := range qs {
			d.Answers[id] = answers[id]
		}
		return d, nil
	}
}

func jevDown(context.Context, any, loopkit.Questions) (*loopkit.Decision, error) {
	return nil, errors.New("reflex unavailable")
}

func mirrorWith(sink *sendSink, decide func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error), p policy.Policy) *operatorMirror {
	return newOperatorMirror(zap.NewNop(), sink.send, decide, p)
}

// A routine notification is logged to the app and goes no further, which is
// the whole of the operator's "approvals and urgent only".
func TestARoutineNotificationStaysInTheApp(t *testing.T) {
	sink := &sendSink{}
	m := mirrorWith(sink, jevAnswers(nil, map[string]loopkit.Answer{"urgency": {Score: 1}}), policy.Default())
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorNotification, Title: "Daily briefing", Body: "3 meetings today"})
	if n := len(sink.all()); n != 0 {
		t.Fatalf("a routine notification reached WhatsApp: %v", sink.all())
	}
}

// Something that cannot wait reaches WhatsApp: marked urgent by whoever raised
// it, or scored at the threshold by the screener.
func TestAnUrgentNotificationReachesWhatsApp(t *testing.T) {
	sink := &sendSink{}
	asked := 0
	m := mirrorWith(sink, jevAnswers(&asked, map[string]loopkit.Answer{"urgency": {Score: 3}}), policy.Default())
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorNotification, Title: "Brain is down", Body: "no replies until it recovers"})
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorNotification, Title: "Prod 5xx", Body: "alarm firing", Urgent: true})
	got := sink.all()
	if len(got) != 2 || got[0] != "Brain is down\n\nno replies until it recovers" {
		t.Fatalf("sent %q", got)
	}
	if asked != 1 {
		t.Errorf("Jev was asked %d times; a notification marked urgent needs no second opinion", asked)
	}
}

// An unanswered urgency question is not a reason to interrupt: the item is in
// the app already, and noise is the failure the operator named.
func TestAnUnclassifiedNotificationStaysInTheApp(t *testing.T) {
	sink := &sendSink{}
	m := mirrorWith(sink, jevDown, policy.Default())
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorNotification, Title: "t", Body: "b"})
	if n := len(sink.all()); n != 0 {
		t.Fatalf("sent %d, want 0", n)
	}
}

// An approval on the hard floor reaches WhatsApp with what to reply.
func TestAHardFloorApprovalReachesWhatsApp(t *testing.T) {
	sink := &sendSink{}
	asked := 0
	m := mirrorWith(sink, jevAnswers(&asked, nil), policy.Default())
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorApproval, Title: "Pay invoice", Body: "pay 5", ProposalID: "p-1",
		Floor: "payments or spending money"})
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("sent %d, want 1", len(got))
	}
	for _, want := range []string{"Approval needed: Pay invoice", "pay 5", "payments or spending money", "Proposal id: p-1", "approve or reject"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("approval text %q missing %q", got[0], want)
		}
	}
	if asked != 0 {
		t.Error("an approval that names its floor rule needs no classification")
	}
}

// A loop's proposal does not say whether it is on the floor; Jev is asked, and
// one that is not stays in the app's approvals inbox.
func TestAnApprovalOffTheFloorStaysInTheApp(t *testing.T) {
	sink := &sendSink{}
	m := mirrorWith(sink, jevAnswers(nil, map[string]loopkit.Answer{"floor": {Noul: 0.1}}), policy.Default())
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorApproval, Title: "Reply to Dana?", Body: "draft", ProposalID: "p-2"})
	if n := len(sink.all()); n != 0 {
		t.Fatalf("an off-floor approval reached WhatsApp: %v", sink.all())
	}
	m = mirrorWith(sink, jevAnswers(nil, map[string]loopkit.Answer{"floor": {Noul: 0.9}}), policy.Default())
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorApproval, Title: "Delete the staging bucket", Body: "x", ProposalID: "p-3"})
	if n := len(sink.all()); n != 1 {
		t.Fatalf("an on-floor approval did not reach WhatsApp: %v", sink.all())
	}
}

// approvals records what the policy approved, and whether the phone was told
// an approval is waiting.
type approvals struct {
	mu       sync.Mutex
	approved []string
	notes    []string
	pushed   int
	fail     error
}

func (a *approvals) approve(id, note string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail != nil {
		return a.fail
	}
	a.approved = append(a.approved, id)
	a.notes = append(a.notes, note)
	return nil
}

func (a *approvals) push() {
	a.mu.Lock()
	a.pushed++
	a.mu.Unlock()
}

func actingMirror(sink *sendSink, decide func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error), p policy.Policy) (*operatorMirror, *approvals) {
	m := mirrorWith(sink, decide, p)
	a := &approvals{}
	m.approve = a.approve
	return m, a
}

func (a *approvals) proposal(id, title string, floor string) builtin.MirrorEvent {
	return builtin.MirrorEvent{Kind: builtin.MirrorApproval, Title: title, Body: "draft", Source: "proposal:task",
		ProposalID: id, Floor: floor, Push: a.push}
}

// Under "act", a proposal from a loop that is not on the hard floor is not
// left unseen in the app's inbox: it is approved by the policy and carried
// out through the same path the operator's approval takes. The operator is
// not asked, on WhatsApp or by a phone push; what it sends is reported after.
func TestAnOffFloorProposalIsCarriedOutUnderAct(t *testing.T) {
	sink := &sendSink{}
	m, a := actingMirror(sink, jevAnswers(nil, map[string]loopkit.Answer{"floor": {Noul: 0.1}}), policy.Default())
	m.handle(a.proposal("p-10", "Reply to Dana with the dates", ""))
	if len(a.approved) != 1 || a.approved[0] != "p-10" {
		t.Fatalf("approved %v, want the off-floor proposal", a.approved)
	}
	if !strings.Contains(a.notes[0], "hard floor") {
		t.Errorf("the approval's note %q does not say why no one was asked", a.notes[0])
	}
	if n := len(sink.all()); n != 0 || a.pushed != 0 {
		t.Errorf("the operator was asked about it: %d WhatsApp, %d push", n, a.pushed)
	}
}

// A proposal on the hard floor still waits for the operator — whether its
// raiser named the rule or Jev placed it there, or Jev could not say.
func TestAHardFloorProposalStillWaitsForTheOperator(t *testing.T) {
	for name, c := range map[string]struct {
		decide func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error)
		floor  string
	}{
		"named":          {jevAnswers(nil, nil), "payments or spending money"},
		"classified":     {jevAnswers(nil, map[string]loopkit.Answer{"floor": {Noul: 0.9}}), ""},
		"unsure":         {jevAnswers(nil, map[string]loopkit.Answer{"floor": {Noul: 0.4}}), ""},
		"unclassifiable": {jevDown, ""},
	} {
		sink := &sendSink{}
		m, a := actingMirror(sink, c.decide, policy.Default())
		m.handle(a.proposal("p-11", "Pay the invoice", c.floor))
		if len(a.approved) != 0 {
			t.Errorf("%s: a hard-floor proposal was approved by the policy", name)
		}
		if n := len(sink.all()); n != 1 || a.pushed != 1 {
			t.Errorf("%s: the operator was not asked: %d WhatsApp, %d push", name, n, a.pushed)
		}
	}
}

// Under "ask first" every proposal is the operator's to decide.
func TestAskFirstNeverCarriesOutAProposal(t *testing.T) {
	sink := &sendSink{}
	m, a := actingMirror(sink, jevAnswers(nil, map[string]loopkit.Answer{"floor": {Noul: 0}}), policy.New(policy.Config{Autonomy: "ask"}))
	m.handle(a.proposal("p-12", "Reply to Dana", ""))
	if len(a.approved) != 0 || a.pushed != 1 {
		t.Errorf("ask-first: approved %v, pushed %d; want it left for the operator", a.approved, a.pushed)
	}
}

// Carrying out an off-floor proposal does not depend on a WhatsApp channel.
func TestAnOffFloorProposalIsCarriedOutWithoutWhatsApp(t *testing.T) {
	m := newOperatorMirror(zap.NewNop(), nil, jevAnswers(nil, map[string]loopkit.Answer{"floor": {Noul: 0.1}}), policy.Default())
	a := &approvals{}
	m.approve = a.approve
	m.handle(a.proposal("p-13", "Reply to Dana", ""))
	if len(a.approved) != 1 {
		t.Fatalf("with no WhatsApp channel the proposal was left: %v", a.approved)
	}
}

// When the policy's approval cannot go through (no agent, or someone decided
// it first) nothing is sent to WhatsApp for an off-floor proposal; one that
// still waits keeps its phone push.
func TestAnOffFloorProposalThatCannotBeCarriedOutWaitsInTheApp(t *testing.T) {
	sink := &sendSink{}
	m, a := actingMirror(sink, jevAnswers(nil, map[string]loopkit.Answer{"floor": {Noul: 0.1}}), policy.Default())
	a.fail = errors.New("no agent available")
	m.handle(a.proposal("p-14", "Reply to Dana", ""))
	if n := len(sink.all()); n != 0 {
		t.Errorf("an off-floor proposal reached WhatsApp: %v", sink.all())
	}
	if a.pushed != 1 {
		t.Errorf("a proposal left waiting in the app was not pushed to the phone (%d)", a.pushed)
	}

	// Decided by someone else first: nothing is waiting, so nothing is pushed.
	a.fail, a.pushed = fmt.Errorf("%w: approved", api.ErrAlreadyDecided), 0
	m.handle(a.proposal("p-15", "Reply to Dana", ""))
	if a.pushed != 0 || len(sink.all()) != 0 {
		t.Errorf("a proposal already decided was announced: %d push, %v", a.pushed, sink.all())
	}
}

// An approval waits on a person, so one that cannot be classified is sent.
func TestAnUnclassifiedApprovalIsSent(t *testing.T) {
	sink := &sendSink{}
	m := mirrorWith(sink, jevDown, policy.Default())
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorApproval, Title: "t", Body: "b", ProposalID: "p-4"})
	if n := len(sink.all()); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
}

// Under "ask first" every approval is the operator's to give.
func TestAskFirstSendsEveryApproval(t *testing.T) {
	sink := &sendSink{}
	m := mirrorWith(sink, jevAnswers(nil, map[string]loopkit.Answer{"floor": {Noul: 0}}), policy.New(policy.Config{Autonomy: "ask"}))
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorApproval, Title: "Reply to Dana?", Body: "draft", ProposalID: "p-5"})
	if n := len(sink.all()); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
}

// An operator who turns WhatsApp off gets nothing there, however urgent.
func TestAnEmptyWhatsAppPolicySendsNothing(t *testing.T) {
	sink := &sendSink{}
	m := mirrorWith(sink, jevAnswers(nil, nil), policy.New(policy.Config{WhatsApp: []string{}}))
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorNotification, Title: "t", Body: "b", Urgent: true})
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorApproval, Title: "t", Body: "b", ProposalID: "p", Floor: "payments"})
	if n := len(sink.all()); n != 0 {
		t.Fatalf("sent %d, want 0", n)
	}
}

func TestMirrorHookIsAsync(t *testing.T) {
	release := make(chan struct{})
	sink := &sendSink{}
	m := mirrorWith(sink, func(ctx context.Context, _ any, _ loopkit.Questions) (*loopkit.Decision, error) {
		<-release
		return nil, errors.New("x")
	}, policy.Default())
	done := make(chan struct{})
	go func() {
		m.Hook(builtin.MirrorEvent{Kind: builtin.MirrorNotification, Title: "t", Body: "b"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Hook blocked on Jev")
	}
	close(release)
}

func TestMirrorSendIsNotEchoedBackToTheApp(t *testing.T) {
	var pushed []string
	var m *operatorMirror
	notify := func(target, content string) {}
	send := func(text string) error {
		// what comms.Manager.Send does after a successful send
		notify("operator-alias", text)
		return nil
	}
	m = newOperatorMirror(zap.NewNop(), send, jevDown, policy.Default())
	notify = m.Notifier(func(target, content string) { pushed = append(pushed, content) })

	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorNotification, Title: "t", Body: "b", Urgent: true})
	if len(pushed) != 0 {
		t.Fatalf("mirror send was pushed back to the app: %v", pushed)
	}
	notify("someone", "hello from the agent")
	if len(pushed) != 1 {
		t.Fatalf("ordinary proactive send must still push; got %v", pushed)
	}
}

func TestProposalDecideGateAndExecution(t *testing.T) {
	isOp := func(_, chat string) bool { return chat == "op" }
	mk := func(kind bus.EventKind, chat string) bus.Event {
		return bus.NewEvent(kind, "a", map[string]any{"channel_id": chat})
	}
	if !isOperatorTurn(mk(bus.EventCommsMessage, "op"), isOp) || !isOperatorTurn(mk("api.chat", ""), isOp) {
		t.Error("operator turns must be allowed")
	}
	for _, evt := range []bus.Event{
		mk(bus.EventCommsMessage, "stranger"), mk(bus.EventCommsMessage, ""),
		mk("timer.fired", "op"), mk(bus.EventCommsSent, "op"),
	} {
		if isOperatorTurn(evt, isOp) {
			t.Errorf("%s from %v must not be an operator turn", evt.Kind, evt.Payload)
		}
	}
	if (&KarmaxRuntime{}).operatorTurnTools(mk(bus.EventCommsMessage, "op")) != nil {
		t.Error("no console means no tool")
	}

	s, err := store.New(filepath.Join(t.TempDir(), "k.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	id, err := builtin.CreateProposal(s, "", "task", "Do thing", "", "act", "normal")
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	var gotID, gotDec, gotBy string
	tool := &proposalDecideTool{store: s, decide: func(i, d, n, by string) error {
		gotID, gotDec, gotBy = i, d, by
		return s.DecideProposalBy(i, "approved", n, by)
	}}
	if r, _ := tool.Execute(context.Background(), map[string]any{"id": id, "decision": "maybe"}); !r.IsError {
		t.Error("bad decision accepted")
	}
	if r, _ := tool.Execute(context.Background(), map[string]any{"id": "nope", "decision": "approve"}); !r.IsError {
		t.Error("unknown id accepted")
	}
	if r, _ := tool.Execute(context.Background(), map[string]any{"id": id, "decision": "approve"}); r.IsError {
		t.Fatalf("approve failed: %+v", r)
	}
	if gotID != id || gotDec != "approve" || gotBy == "" {
		t.Errorf("decide got %q %q %q", gotID, gotDec, gotBy)
	}
	if r, _ := tool.Execute(context.Background(), map[string]any{"id": id, "decision": "reject"}); !r.IsError {
		t.Error("an already-decided proposal was decided again")
	}
}
