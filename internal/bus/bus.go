package bus

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The durable event log.
//
// What this replaces dropped events on the floor: a subscriber that was busy
// when something happened got a warning in the log and nothing else. Loop runs
// were made durable, but the events that TRIGGER them were not, so the guarantee
// stopped one layer short of where it mattered.
//
// Here an event is written to disk before any subscriber sees it, and each
// subscriber records how far it has read. A crash resumes from that offset
// rather than starting blind. Delivery is at-least-once: the offset advances
// only after a handler returns, so a process that dies mid-handler redelivers
// rather than losing the event. Handlers must therefore tolerate seeing the
// same event twice, which is the honest trade — exactly-once across a process
// boundary is not something a queue can promise.

const (
	// batchSize bounds one read. Large enough that catching up after downtime
	// is not a round trip per event, small enough that a slow handler does not
	// hold a batch of thousands in memory.
	batchSize = 128
	// idlePoll is the backstop when no publish wakes a consumer — a wake-up can
	// be missed if a publisher dies between the append and the notify.
	idlePoll = 5 * time.Second
	// maxAttempts before an event is dead-lettered and the subscriber moves on.
	// Retrying forever means one poisonous event stops every event behind it.
	maxAttempts = 3
)

// Handler processes one event. Returning nil advances the subscriber's offset;
// returning an error retries, and then dead-letters.
type Handler func(context.Context, Event) error

// Journal is the durable storage the log needs, narrowed to what it uses.
type Journal interface {
	AppendLogEvent(store.LogEvent) (int64, error)
	LogEventsAfter(workspace string, after int64, kinds []string, limit int) ([]store.LogEvent, error)
	ConsumerOffset(name, workspace string) (int64, error)
	SetConsumerOffset(name, workspace string, seq int64) error
	RecordDeadLetter(store.DeadLetter) error
}

// Log is the append-only event log and its subscribers.
type Log struct {
	journal   Journal
	workspace string
	log       *zap.Logger

	// onDead is how the operator hears about an event nobody could process.
	onDead func(store.DeadLetter)

	// retryDelay is a field so tests can collapse the backoff rather than
	// spending six real seconds proving that a dead letter happens.
	retryDelay func(attempt int) time.Duration

	// notify is closed and replaced on every publish, so consumers waiting on
	// it wake at once instead of sitting out the poll interval.
	mu     sync.Mutex
	notify chan struct{}
}

func NewLog(j Journal, workspace string, log *zap.Logger) *Log {
	if workspace == "" {
		workspace = store.DefaultWorkspace
	}
	return &Log{
		journal:    j,
		workspace:  workspace,
		log:        log,
		notify:     make(chan struct{}),
		retryDelay: backoff,
	}
}

// OnDeadLetter installs the notifier for events that were given up on.
func (l *Log) OnDeadLetter(fn func(store.DeadLetter)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onDead = fn
}

// Publish appends an event. It is durable once this returns nil.
//
// The error is real and worth handling: a failed append means no subscriber
// will ever see this event, which is the failure the log exists to prevent.
func (l *Log) Publish(e Event) error {
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if _, err := l.journal.AppendLogEvent(store.LogEvent{
		ID:        e.ID,
		Workspace: l.workspace,
		Kind:      string(e.Kind),
		AgentID:   e.AgentID,
		Payload:   e.Payload,
		Meta:      e.Meta,
		CreatedAt: e.Timestamp,
	}); err != nil {
		l.log.Error("event could not be recorded; no subscriber will see it",
			zap.String("kind", string(e.Kind)), zap.Error(err))
		return fmt.Errorf("bus: append %s: %w", e.Kind, err)
	}
	l.wake()
	return nil
}

// Consume runs a durable subscriber until ctx is cancelled, resuming from where
// this name last got to. The name is the identity of the offset, so it must be
// stable across restarts and unique per subscriber.
func (l *Log) Consume(ctx context.Context, name string, kinds []EventKind, h Handler) {
	go l.consume(ctx, name, kinds, h)
}

func (l *Log) consume(ctx context.Context, name string, kinds []EventKind, h Handler) {
	filter := make([]string, 0, len(kinds))
	for _, k := range kinds {
		filter = append(filter, string(k))
	}

	offset, err := l.journal.ConsumerOffset(name, l.workspace)
	if err != nil {
		// Starting from zero would replay everything retained, which is worse
		// than waiting for a read that works.
		l.log.Error("could not read subscriber offset; not starting",
			zap.String("subscriber", name), zap.Error(err))
		return
	}
	l.log.Info("event subscriber started",
		zap.String("subscriber", name), zap.Int64("from_seq", offset))

	for {
		// Taken BEFORE the read: a publish landing between the read and the
		// wait would otherwise be missed until the poll interval expired.
		waiter := l.waiter()

		batch, err := l.journal.LogEventsAfter(l.workspace, offset, filter, batchSize)
		if err != nil {
			l.log.Warn("could not read the event log",
				zap.String("subscriber", name), zap.Error(err))
			if !sleepOrDone(ctx, idlePoll) {
				return
			}
			continue
		}

		for _, rec := range batch {
			if !l.deliver(ctx, name, h, rec) {
				return // cancelled mid-event; it stays unacked and is redelivered
			}
			offset = rec.Seq
			if err := l.journal.SetConsumerOffset(name, l.workspace, offset); err != nil {
				l.log.Warn("could not record subscriber progress",
					zap.String("subscriber", name), zap.Error(err))
			}
		}
		if len(batch) > 0 {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-waiter:
		case <-time.After(idlePoll):
		}
	}
}

// deliver hands one event to a handler, retrying before giving up. It reports
// false only when cancelled, which leaves the event unacked on purpose.
func (l *Log) deliver(ctx context.Context, name string, h Handler, rec store.LogEvent) bool {
	e := eventFrom(rec)
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		err := call(ctx, h, e)
		if err == nil {
			return true
		}
		lastErr = err
		l.log.Warn("event handler failed",
			zap.String("subscriber", name), zap.String("kind", rec.Kind),
			zap.Int("attempt", attempt), zap.Error(err))
		if attempt < maxAttempts && !sleepOrDone(ctx, l.retryDelay(attempt)) {
			return false
		}
	}

	dead := store.DeadLetter{
		Subscriber: name, EventSeq: rec.Seq, EventID: rec.ID,
		Kind: rec.Kind, Attempts: maxAttempts, LastError: lastErr.Error(),
	}
	if err := l.journal.RecordDeadLetter(dead); err != nil {
		l.log.Error("could not record a dead-lettered event",
			zap.String("subscriber", name), zap.Error(err))
	}
	// Loud: an event nobody could handle means something did not happen, and
	// the whole point of the log is that this stops being silent.
	l.log.Error("event dead-lettered; the work it would have triggered did NOT happen",
		zap.String("subscriber", name), zap.String("kind", rec.Kind),
		zap.String("event_id", rec.ID), zap.Error(lastErr))

	l.mu.Lock()
	fn := l.onDead
	l.mu.Unlock()
	if fn != nil {
		fn(dead)
	}
	return true
}

// call isolates handler panics. A subscriber that dies on one bad payload takes
// every later event with it, so a panic is turned into a retryable error.
func call(ctx context.Context, h Handler, e Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return h(ctx, e)
}

func eventFrom(rec store.LogEvent) Event {
	return Event{
		ID:        rec.ID,
		Kind:      EventKind(rec.Kind),
		AgentID:   rec.AgentID,
		Timestamp: rec.CreatedAt,
		Payload:   rec.Payload,
		Meta:      rec.Meta,
		Seq:       rec.Seq,
	}
}

func backoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return time.Second
	case 2:
		return 5 * time.Second
	default:
		return 15 * time.Second
	}
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (l *Log) wake() {
	l.mu.Lock()
	defer l.mu.Unlock()
	close(l.notify)
	l.notify = make(chan struct{})
}

func (l *Log) waiter() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.notify
}
