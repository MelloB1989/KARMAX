package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
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

func jevSays(choice string) func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error) {
	return func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error) {
		return &loopkit.Decision{Answers: map[string]loopkit.Answer{"deliver": {Choice: choice, Confidence: 0.9}}}, nil
	}
}

func TestMirrorJevNoIsNotSent(t *testing.T) {
	sink := &sendSink{}
	m := newOperatorMirror(zap.NewNop(), sink.send, jevSays("app_only"))
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorNotification, Title: "t", Body: "b"})
	if n := len(sink.all()); n != 0 {
		t.Fatalf("sent %d, want 0", n)
	}
}

func TestMirrorJevYesIsSentWithTemplate(t *testing.T) {
	sink := &sendSink{}
	m := newOperatorMirror(zap.NewNop(), sink.send, jevSays("whatsapp_now"))
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorNotification, Title: "Build done", Body: "all green"})
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorApproval, Title: "Pay invoice", Body: "pay 5", ProposalID: "p-1"})
	got := sink.all()
	if len(got) != 2 {
		t.Fatalf("sent %d, want 2", len(got))
	}
	if got[0] != "Build done\n\nall green" {
		t.Errorf("notification text = %q", got[0])
	}
	for _, want := range []string{"Approval needed: Pay invoice", "pay 5", "Proposal id: p-1", "approve or reject"} {
		if !strings.Contains(got[1], want) {
			t.Errorf("approval text %q missing %q", got[1], want)
		}
	}
}

func TestMirrorJevUnavailableFailsOpen(t *testing.T) {
	sink := &sendSink{}
	m := newOperatorMirror(zap.NewNop(), sink.send, func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error) {
		return nil, errors.New("reflex unavailable")
	})
	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorNotification, Title: "t", Body: "b"})
	if n := len(sink.all()); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
}

func TestMirrorHookIsAsync(t *testing.T) {
	release := make(chan struct{})
	sink := &sendSink{}
	m := newOperatorMirror(zap.NewNop(), sink.send, func(ctx context.Context, _ any, _ loopkit.Questions) (*loopkit.Decision, error) {
		<-release
		return nil, errors.New("x")
	})
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
	m = newOperatorMirror(zap.NewNop(), send, jevSays("whatsapp_now"))
	notify = m.Notifier(func(target, content string) { pushed = append(pushed, content) })

	m.handle(builtin.MirrorEvent{Kind: builtin.MirrorNotification, Title: "t", Body: "b"})
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
