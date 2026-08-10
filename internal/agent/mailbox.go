package agent

import (
	"context"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"go.uber.org/zap"
)

// Per-conversation mailboxes.
//
// One inbox worker handled one event to completion, so a claude_code.call that
// took eight minutes left the agent deaf to every other chat, loop and mesh
// peer for eight minutes. Ordering was the reason it was serial, but ordering
// only matters WITHIN a conversation: two WhatsApp chats have no order between
// them, and neither does a chat and a scheduled job.
//
// So events are keyed by conversation. Each key gets its own worker, sequential
// within itself and concurrent with every other.

const (
	// mailboxDepth is per conversation. Small on purpose: a chat with hundreds
	// of queued messages is a chat where answering the oldest first is wrong.
	mailboxDepth = 32
	// maxConversations bounds concurrent workers, so a flood of distinct chats
	// cannot spawn unbounded goroutines each holding a model call.
	maxConversations = 64
	// idleTimeout retires a worker whose conversation has gone quiet.
	idleTimeout = 5 * time.Minute
)

type mailboxes struct {
	ctx     context.Context
	handler func(bus.Event)
	log     *zap.Logger

	mu    sync.Mutex
	boxes map[string]chan bus.Event
}

func newMailboxes(ctx context.Context, log *zap.Logger, handler func(bus.Event)) *mailboxes {
	return &mailboxes{
		ctx:     ctx,
		handler: handler,
		log:     log,
		boxes:   make(map[string]chan bus.Event),
	}
}

// deliver routes an event to its conversation, reporting false when that
// conversation is too far behind to take it.
func (m *mailboxes) deliver(e bus.Event) bool {
	key := conversationKey(e)

	m.mu.Lock()
	ch, ok := m.boxes[key]
	if !ok {
		if len(m.boxes) >= maxConversations {
			m.mu.Unlock()
			m.log.Warn("too many active conversations to start another",
				zap.String("conversation", key), zap.Int("active", maxConversations))
			return false
		}
		ch = make(chan bus.Event, mailboxDepth)
		m.boxes[key] = ch
		go m.work(key, ch)
	}
	m.mu.Unlock()

	select {
	case ch <- e:
		return true
	default:
		return false
	}
}

func (m *mailboxes) work(key string, ch chan bus.Event) {
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-m.ctx.Done():
			m.retire(key)
			return

		case e := <-ch:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			m.handler(e)
			idle.Reset(idleTimeout)

		case <-idle.C:
			// Retire only if nothing arrived in the meantime; the delivering
			// goroutine holds the same lock, so this cannot race a new event
			// into a channel nobody is reading.
			m.mu.Lock()
			if len(ch) > 0 {
				m.mu.Unlock()
				idle.Reset(idleTimeout)
				continue
			}
			delete(m.boxes, key)
			m.mu.Unlock()
			return
		}
	}
}

func (m *mailboxes) retire(key string) {
	m.mu.Lock()
	delete(m.boxes, key)
	m.mu.Unlock()
}

// active reports how many conversations are in flight, for the status view.
func (m *mailboxes) active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.boxes)
}

// conversationKey decides what must stay in order.
//
// Two messages in one chat must be answered in the order they arrived. Two
// messages in different chats must not wait for each other, and a scheduled job
// has no ordering relationship with either.
func conversationKey(e bus.Event) string {
	switch e.Kind {
	case bus.EventCommsMessage, bus.EventCommsSent:
		if id, _ := e.Payload["channel_id"].(string); id != "" {
			return "chat:" + id
		}
	case bus.EventScheduledJob:
		if id, _ := e.Payload["job_id"].(string); id != "" {
			return "job:" + id
		}
	case bus.EventWebhookFired:
		if route, _ := e.Payload["route"].(string); route != "" {
			return "hook:" + route
		}
	case bus.EventTimerFired:
		if id, _ := e.Payload["timer_id"].(string); id != "" {
			return "timer:" + id
		}
	}
	// Anything unkeyed shares one lane, which keeps its relative order and
	// still runs alongside the conversations that matter.
	return "kind:" + string(e.Kind)
}
