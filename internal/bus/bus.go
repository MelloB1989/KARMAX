package bus

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

type Bus struct {
	mu   sync.RWMutex
	subs map[string]*Subscription
	log  *zap.Logger
}

func New(log *zap.Logger) *Bus {
	return &Bus{
		subs: make(map[string]*Subscription),
		log:  log,
	}
}

func (b *Bus) Publish(e Event) {
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	for id, sub := range b.subs {
		if !sub.matches(e.Kind) {
			continue
		}
		select {
		case sub.Ch <- e:
		default:
			b.log.Warn("event dropped for slow subscriber", zap.String("sub_id", id), zap.String("event_kind", string(e.Kind)))
		}
	}
}

func (b *Bus) Subscribe(filters ...EventKind) (*Subscription, func()) {
	sub := &Subscription{
		ID:      uuid.New().String(),
		Filters: filters,
		Ch:      make(chan Event, 256),
	}

	b.mu.Lock()
	b.subs[sub.ID] = sub
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		delete(b.subs, sub.ID)
		b.mu.Unlock()
	}

	return sub, cancel
}
