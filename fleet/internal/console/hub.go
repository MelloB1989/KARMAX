package console

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

const (
	hubInterval = 100 * time.Millisecond // at most 10 rebuilds a second
	subBuffer   = 64
)

// Hub rebuilds the snapshot when a source changes and fans the patches out.
type Hub struct {
	fill     func(*Snapshot)
	src      *Sources
	interval time.Duration
	kick     chan struct{}

	mu   sync.Mutex
	seq  uint64
	cur  Snapshot
	json []byte
	subs map[chan Message]struct{}
}

func NewHub(src *Sources) *Hub { return newHub(src.Fill, hubInterval, src) }

func newHub(fill func(*Snapshot), interval time.Duration, src *Sources) *Hub {
	return &Hub{fill: fill, src: src, interval: interval, kick: make(chan struct{}, 1), subs: map[chan Message]struct{}{}}
}

// Changed asks for a rebuild; it never blocks.
func (h *Hub) Changed() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// Run starts the sources (when there are any) and rebuilds until ctx is done.
func (h *Hub) Run(ctx context.Context) {
	if h.src != nil {
		go h.src.Run(ctx, h.Changed)
	}
	h.rebuild()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.kick:
		}
		h.rebuild()
		select { // batch: whatever changed meanwhile is one rebuild
		case <-ctx.Done():
			return
		case <-time.After(h.interval):
		}
	}
}

func (h *Hub) rebuild() {
	var s Snapshot
	h.fill(&s)
	s.Build = BuildID()
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.json == nil {
		h.cur, h.json = s, b
		h.seq = 1
		return
	}
	ops, err := Diff(h.json, b)
	if err != nil || len(ops) == 0 {
		return
	}
	h.cur, h.json = s, b
	h.seq++
	msg := Message{Type: "patch", Seq: h.seq, Ops: ops}
	for ch := range h.subs {
		select {
		case ch <- msg:
		default: // too slow: drop it; the client resubscribes for a fresh snapshot
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// Snapshot is the latest one.
func (h *Hub) Snapshot() Snapshot {
	h.mu.Lock()
	have := h.json != nil
	h.mu.Unlock()
	if !have {
		h.rebuild()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur
}

// Subscribe returns a channel that carries the snapshot first, then each
// patch. The channel is closed when the subscriber is dropped for being slow;
// the returned func unsubscribes.
func (h *Hub) Subscribe(ctx context.Context) (<-chan Message, func()) {
	h.Snapshot() // make sure there is one
	ch := make(chan Message, subBuffer)
	h.mu.Lock()
	snap := h.cur
	ch <- Message{Type: "snapshot", Seq: h.seq, Snapshot: &snap}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	stop := make(chan struct{})
	cancel := func() {
		once.Do(func() {
			close(stop)
			h.mu.Lock()
			if _, ok := h.subs[ch]; ok {
				delete(h.subs, ch)
				close(ch)
			}
			h.mu.Unlock()
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-stop:
		}
	}()
	return ch, cancel
}
