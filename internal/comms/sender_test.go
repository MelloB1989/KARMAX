package comms

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// Who sent a message travels with it to everything that reads the event — the
// agent, the reflex screen, every loop — so none of them has to work it out
// again from a name.
func TestTheEventSaysWhoSentTheMessage(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "karmax.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()
	b := bus.NewLog(s, store.DefaultWorkspace, zap.NewNop())
	mgr := NewManager(b, s, zap.NewNop())
	ch := newFakeChannel("whatsapp-main", "whatsapp")
	if err := mgr.Register(ch, "agent-main"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	events := consume(t, b, bus.EventCommsMessage)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := mgr.StartAll(ctx); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	ch.inbox <- Message{
		ID: "in-1", ChannelID: "120363000000000001@g.us", ChannelType: "whatsapp",
		SenderID: "123456789", SenderName: "Priya", Direction: Inbound, Content: "keep it at 2pm",
		Timestamp: time.Now(),
		Metadata: map[string]any{
			"is_group": true, "chat_name": "Contoso founders",
			"sender_jid": "123456789:4@lid", "sender_role": "operator",
			"sender_is_operator": true, "sender_label": "Omar",
		},
	}
	evt := waitForEvent(t, events, bus.EventCommsMessage)
	for k, want := range map[string]any{
		"sender_name": "Priya", "sender_jid": "123456789:4@lid", "sender_role": "operator",
		"sender_is_operator": true, "sender_label": "Omar", "chat_name": "Contoso founders",
	} {
		if evt.Payload[k] != want {
			t.Errorf("payload[%q] = %v, want %v", k, evt.Payload[k], want)
		}
	}
}
