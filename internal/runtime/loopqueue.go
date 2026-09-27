package runtime

import (
	"context"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// Events that arrive while a loop is already running.
//
// The single-flight lease exists because a four-minute run on a two-minute
// schedule used to pile up and answer the same WhatsApp message twice. For a
// SCHEDULE that guard is free: skipping a tick loses nothing, because the next
// tick does the same work.
//
// For an EVENT it was destroying data. The trigger carries a message that
// exists nowhere else in the pipeline — the bus offset has already advanced —
// so returning early threw it away permanently. On the worst day that was 1559
// dropped WhatsApp messages, and the failure the operator saw was an assistant
// they had tagged by name sitting silent.
//
// So an event waits instead. One pending trigger per conversation, latest
// winning: two messages in one chat while a pass is in flight become one
// follow-up pass, which is what the lease was protecting against in the first
// place, while two messages in different chats both survive.

// maxPendingPerLoop bounds the queue. A loop far enough behind that this many
// distinct conversations are waiting has a problem no queue fixes, and an
// unbounded map here would be a way to run the daemon out of memory.
const maxPendingPerLoop = 256

// pendingTriggers holds what arrived during a run.
type pendingTriggers struct {
	mu     sync.Mutex
	byLoop map[string]map[string]loopkit.Trigger
	order  map[string][]string
}

func newPendingTriggers() *pendingTriggers {
	return &pendingTriggers{
		byLoop: make(map[string]map[string]loopkit.Trigger),
		order:  make(map[string][]string),
	}
}

// add records a trigger to run once the loop is free, reporting whether it was
// kept. A second trigger for the same conversation replaces the first.
func (p *pendingTriggers) add(loop string, t loopkit.Trigger) bool {
	key := conversationOf(t)

	p.mu.Lock()
	defer p.mu.Unlock()

	queue, ok := p.byLoop[loop]
	if !ok {
		queue = make(map[string]loopkit.Trigger)
		p.byLoop[loop] = queue
	}
	if _, already := queue[key]; !already {
		if len(queue) >= maxPendingPerLoop {
			return false
		}
		p.order[loop] = append(p.order[loop], key)
	}
	// Latest wins: the newer message is the one worth answering, and the pass
	// re-reads the thread anyway.
	queue[key] = t
	return true
}

// take removes and returns everything waiting for a loop, oldest conversation
// first so a chat that has waited longest is not starved by a busier one.
func (p *pendingTriggers) take(loop string) []loopkit.Trigger {
	p.mu.Lock()
	defer p.mu.Unlock()

	queue := p.byLoop[loop]
	if len(queue) == 0 {
		delete(p.byLoop, loop)
		delete(p.order, loop)
		return nil
	}
	out := make([]loopkit.Trigger, 0, len(queue))
	for _, key := range p.order[loop] {
		if t, ok := queue[key]; ok {
			out = append(out, t)
		}
	}
	delete(p.byLoop, loop)
	delete(p.order, loop)
	return out
}

// waiting reports how many conversations are queued, for the status view.
func (p *pendingTriggers) waiting(loop string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byLoop[loop])
}

// conversationOf decides what coalesces with what.
//
// Two messages in one chat must not produce two replies — that is the whole
// reason the lease is here. Two messages in different chats have no such
// relationship and must both be answered.
func conversationOf(t loopkit.Trigger) string {
	for _, key := range []string{"channel_id", "chat_id", "chat"} {
		if id, _ := t.Payload[key].(string); id != "" {
			return key + ":" + id
		}
	}
	return "kind:" + t.Kind
}

// drainPending runs whatever queued up while a loop held the lease.
//
// Recursion is deliberate and safe: each drained trigger takes the lease in
// turn, and anything arriving during THAT run queues behind it again. The
// queue is emptied before the runs start, so a loop that is permanently busy
// cannot build a chain that never unwinds.
func (rt *KarmaxRuntime) drainPending(parent context.Context, l loopkit.Loop) {
	waiting := rt.pending.take(l.Name)
	if len(waiting) == 0 {
		return
	}
	if parent.Err() != nil {
		rt.log.Warn("dropping queued events because the daemon is shutting down",
			zap.String("loop", l.Name), zap.Int("events", len(waiting)))
		return
	}
	rt.log.Info("running events that arrived while the loop was busy",
		zap.String("loop", l.Name), zap.Int("events", len(waiting)))

	for _, t := range waiting {
		// Waiting behind long runs can age a message past the point where
		// answering it helps anyone.
		if queuedTooLong(t) {
			rt.log.Warn("dropped a queued event that went stale while waiting",
				zap.String("loop", l.Name), zap.String("conversation", conversationOf(t)))
			continue
		}
		go rt.runLoopDurable(parent, l, t, 1)
	}
}

// payloadEventAt carries an event's own time through a loop trigger.
const payloadEventAt = "event_at"

// queuedTooLong applies the freshness rule to a trigger that has been waiting.
// A trigger with no recorded time is not evidence of age and is run.
func queuedTooLong(t loopkit.Trigger) bool {
	raw, _ := t.Payload[payloadEventAt].(string)
	if raw == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return false
	}
	kind, _ := t.Payload["event_kind"].(string)
	_, stale := staleEvent(bus.Event{Kind: bus.EventKind(kind), Timestamp: at})
	return stale
}

// tooStaleForLoops applies the agent router's freshness rule to loops.
//
// The router has refused stale events for a long time — a conversation is
// perishable, and answering a seven-week-old message is worse than not
// answering it. The loop subscribers never had the same check, so when history
// was replayed onto the bus on 27 Sep the router logged "skipped an event too
// old to act on" for each one while wa-monitor, reading the same events, went
// on to reply to three-month-old conversations in ten chats. Same event, same
// age, opposite outcome, because only one of the two readers asked how old it
// was.
func (rt *KarmaxRuntime) tooStaleForLoops(evt bus.Event) bool {
	age, stale := staleEvent(evt)
	if stale {
		rt.log.Warn("loops skipped an event too old to act on",
			zap.String("kind", string(evt.Kind)), zap.String("event", evt.ID),
			zap.Duration("age", age.Round(time.Minute)))
	}
	return stale
}
