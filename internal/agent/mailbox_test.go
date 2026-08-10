package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"go.uber.org/zap"
)

func chatEvent(chat string, n int) bus.Event {
	return bus.NewEvent(bus.EventCommsMessage, "a", map[string]any{"channel_id": chat, "n": n})
}

// The bug: one slow event made the agent deaf to every other chat.
func TestASlowChatDoesNotBlockAnother(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	fast := make(chan struct{}, 1)
	m := newMailboxes(ctx, zap.NewNop(), func(e bus.Event) {
		if e.Payload["channel_id"] == "slow" {
			<-release
			return
		}
		fast <- struct{}{}
	})

	if !m.deliver(chatEvent("slow", 1)) {
		t.Fatal("delivery to the slow chat was refused")
	}
	// Give the slow worker a moment to actually be in the handler.
	time.Sleep(50 * time.Millisecond)
	if !m.deliver(chatEvent("other", 1)) {
		t.Fatal("delivery to the second chat was refused")
	}

	select {
	case <-fast:
	case <-time.After(3 * time.Second):
		t.Fatal("a stuck conversation blocked an unrelated one")
	}
	close(release)
}

func TestOneChatStaysInOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const n = 50
	var (
		mu   sync.Mutex
		seen []int
	)
	done := make(chan struct{})
	m := newMailboxes(ctx, zap.NewNop(), func(e bus.Event) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, e.Payload["n"].(int))
		if len(seen) == n {
			close(done)
		}
	})

	// Retried on backpressure, as the event log's consumer does — the ordering
	// claim is about what a chat sees, not about the queue never filling.
	for i := 0; i < n; i++ {
		for attempt := 0; !m.deliver(chatEvent("one", i)); attempt++ {
			if attempt > 200 {
				t.Fatalf("delivery %d never accepted", i)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("not everything arrived")
	}

	mu.Lock()
	defer mu.Unlock()
	for i, v := range seen {
		if v != i {
			t.Fatalf("message %d was handled at position %d — a chat was reordered", v, i)
		}
	}
}

func TestABackloggedChatIsReportedNotSilentlyDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	block := make(chan struct{})
	m := newMailboxes(ctx, zap.NewNop(), func(bus.Event) { <-block })

	// One in the handler, mailboxDepth queued, and then it must say no.
	accepted := 0
	for i := 0; i < mailboxDepth+50; i++ {
		if m.deliver(chatEvent("one", i)) {
			accepted++
		}
	}
	if accepted > mailboxDepth+1 {
		t.Errorf("accepted %d events into a chat with depth %d", accepted, mailboxDepth)
	}
	if accepted == 0 {
		t.Error("nothing was accepted at all")
	}
	close(block)
}

func TestConversationsAreKeyedByWhatMustStayOrdered(t *testing.T) {
	cases := []struct {
		name string
		evt  bus.Event
		want string
	}{
		{"a chat", chatEvent("wa-123", 0), "chat:wa-123"},
		{"a scheduled job", bus.NewEvent(bus.EventScheduledJob, "a", map[string]any{"job_id": "j1"}), "job:j1"},
		{"a webhook route", bus.NewEvent(bus.EventWebhookFired, "a", map[string]any{"route": "/hook"}), "hook:/hook"},
		{"a timer", bus.NewEvent(bus.EventTimerFired, "a", map[string]any{"timer_id": "t1"}), "timer:t1"},
		{"anything else", bus.NewEvent(bus.EventDelegationDone, "a", nil), "kind:delegation.completed"},
		{"a chat with no id", bus.NewEvent(bus.EventCommsMessage, "a", nil), "kind:comms.message"},
	}
	for _, tc := range cases {
		if got := conversationKey(tc.evt); got != tc.want {
			t.Errorf("%s: key = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAQuietConversationRetiresItsWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newMailboxes(ctx, zap.NewNop(), func(bus.Event) {})
	m.deliver(chatEvent("one", 0))
	if m.active() != 1 {
		t.Fatalf("active = %d, want 1", m.active())
	}

	// Cancelling is the other way a worker goes away, and it must not leak.
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.active() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("%d workers survived cancellation", m.active())
}
