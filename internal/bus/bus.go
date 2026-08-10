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

// The durable event log. An event is on disk before any subscriber sees it, and
// each subscriber records how far it has read, so a crash resumes rather than
// starting blind.
//
// Delivery is at-least-once — the offset advances only after a handler returns —
// so handlers must tolerate seeing an event twice.

const (
	batchSize = 128
	// idlePoll is the backstop for a wake-up missed between append and notify.
	idlePoll = 5 * time.Second
	// maxAttempts before dead-lettering, so one bad event cannot block the rest.
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

	onDead func(store.DeadLetter)
	// retryDelay is a field so tests can collapse the backoff.
	retryDelay func(attempt int) time.Duration

	// notify is closed and replaced on each publish to wake waiting consumers.
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

// Publish appends an event, durable once this returns nil. A failed append
// means no subscriber will ever see it, so the error is worth handling.
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

// Consume runs a durable subscriber until ctx is cancelled. The name is the
// offset's identity: stable across restarts, unique per subscriber.
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
		// Starting from zero would replay everything retained.
		l.log.Error("could not read subscriber offset; not starting",
			zap.String("subscriber", name), zap.Error(err))
		return
	}
	l.log.Info("event subscriber started",
		zap.String("subscriber", name), zap.Int64("from_seq", offset))

	for {
		waiter := l.waiter() // before the read, or a publish in between is missed

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

// deliver retries before giving up, reporting false only when cancelled, which
// leaves the event unacked on purpose.
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

// call turns a handler panic into a retryable error, so one bad payload cannot
// take every later event with it.
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
