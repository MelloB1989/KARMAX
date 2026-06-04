package comms

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

type fakeChannel struct {
	id      string
	typ     string
	inbox   chan Message
	mu      sync.Mutex
	sent    []sentMessage
	sendErr error
}

type sentMessage struct {
	target  string
	content string
}

func newFakeChannel(id, typ string) *fakeChannel {
	return &fakeChannel{
		id:    id,
		typ:   typ,
		inbox: make(chan Message, 10),
	}
}

func (f *fakeChannel) ID() string { return f.id }

func (f *fakeChannel) Type() string { return f.typ }

func (f *fakeChannel) Start(context.Context) error { return nil }

func (f *fakeChannel) Stop() error { return nil }

func (f *fakeChannel) Send(_ context.Context, target string, content string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMessage{target: target, content: content})
	return nil
}

func (f *fakeChannel) SendEmbed(ctx context.Context, target string, embed Embed) error {
	return f.Send(ctx, target, embed.Description)
}

func (f *fakeChannel) SendFile(context.Context, string, string, []byte) error { return nil }

func (f *fakeChannel) IncomingMessages() <-chan Message { return f.inbox }

func (f *fakeChannel) sentMessages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMessage, len(f.sent))
	copy(out, f.sent)
	return out
}

func TestManagerRoutesAndPersistsCleanOutbound(t *testing.T) {
	b := bus.New(zap.NewNop())
	s, err := store.New(filepath.Join(t.TempDir(), "karmax.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	mgr := NewManager(b, s, zap.NewNop())
	ch := newFakeChannel("discord-main", "discord")
	if err := mgr.Register(ch, "agent-main"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	sub, cancel := b.Subscribe(bus.EventCommsMessage, bus.EventCommsSent)
	defer cancel()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := mgr.StartAll(ctx); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	ch.inbox <- Message{
		ID:          "incoming-1",
		ChannelID:   "discord-target",
		ChannelType: "discord",
		SenderID:    "user-1",
		Direction:   Inbound,
		Content:     "hello",
		Timestamp:   time.Now(),
	}

	inbound := waitForEvent(t, sub.Ch, bus.EventCommsMessage)
	if inbound.Payload["channel_id"] != "discord-target" {
		t.Fatalf("expected inbound target to be captured, got %+v", inbound.Payload)
	}

	if err := mgr.Send("discord-main", "discord-target", "<think>hidden</think>clean reply"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	sent := ch.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("expected one outbound send, got %d", len(sent))
	}
	if sent[0].target != "discord-target" || sent[0].content != "clean reply" {
		t.Fatalf("unexpected outbound message: %+v", sent[0])
	}

	outbound := waitForEvent(t, sub.Ch, bus.EventCommsSent)
	if outbound.Payload["content"] != "clean reply" {
		t.Fatalf("outbound event should contain sanitized content, got %+v", outbound.Payload)
	}

	messages, err := s.ListChannelMessages("discord-target", 10)
	if err != nil {
		t.Fatalf("ListChannelMessages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected inbound and outbound messages persisted, got %d", len(messages))
	}
}

func TestManagerDNDAlertsAlternativeChannel(t *testing.T) {
	b := bus.New(zap.NewNop())
	s, err := store.New(filepath.Join(t.TempDir(), "karmax.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	mgr := NewManager(b, s, zap.NewNop())
	primary := newFakeChannel("discord-main", "discord")
	alt := newFakeChannel("whatsapp-main", "whatsapp")
	if err := mgr.RegisterWithOptions(primary, "agent-main", ChannelOptions{DND: true}); err != nil {
		t.Fatalf("RegisterWithOptions primary: %v", err)
	}
	if err := mgr.Register(alt, "agent-main"); err != nil {
		t.Fatalf("Register alt: %v", err)
	}

	if err := mgr.Send("discord-main", "discord-target", "needs attention"); err == nil {
		t.Fatal("expected DND primary send to fail")
	}

	if got := len(primary.sentMessages()); got != 0 {
		t.Fatalf("primary DND channel should not receive sends, got %d", got)
	}
	sent := alt.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("expected one alternative alert, got %d", len(sent))
	}
	if sent[0].content == "" {
		t.Fatal("alternative alert content should not be empty")
	}
}

func waitForEvent(t *testing.T, ch <-chan bus.Event, kind bus.EventKind) bus.Event {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case evt := <-ch:
			if evt.Kind == kind {
				return evt
			}
		case <-timeout:
			t.Fatal(fmt.Sprintf("timed out waiting for event %s", kind))
		}
	}
}
