package sources

import (
	"strings"
	"sync"

	"github.com/MelloB1989/karmax/internal/bus"
)

// memorySyncedKey is where an event carries MemorySynced on the bus.
const memorySyncedKey = "memory_synced"

// MemorySync names the sources whose conversations a memory syncer reads into
// long-term memory — every message, whoever wrote it.
//
// Such a source's messages reach memory twice over otherwise: once through
// the syncer, which reads the whole conversation, and again through the turn
// each message opens, which the screener may file as "remember" and the agent
// may file with memory.ingest. So a syncer that is running covers its source
// here, and an event from a covered source is marked MemorySynced on its way
// to a turn: it is acted on as before, and filed only by the syncer.
//
// The zero value covers nothing; a nil MemorySync is safe to use and covers
// nothing.
type MemorySync struct {
	mu      sync.RWMutex
	covered map[string]bool
}

// Cover records that a memory syncer reads every conversation of source.
func (m *MemorySync) Cover(source string) {
	source = strings.ToLower(strings.TrimSpace(source))
	if m == nil || source == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.covered == nil {
		m.covered = map[string]bool{}
	}
	m.covered[source] = true
}

// Covers reports whether a memory syncer reads source's conversations.
func (m *MemorySync) Covers(source string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.covered[strings.ToLower(strings.TrimSpace(source))]
}

// Mark returns evt marked MemorySynced when it is an event from a covered
// source, and evt unchanged otherwise. The payload is copied before it is
// marked: the event as published is shared with every other consumer.
func (m *MemorySync) Mark(evt bus.Event) bus.Event {
	src, ok := FromBusEvent(evt)
	if !ok || src.MemorySynced || !m.Covers(src.Source) {
		return evt
	}
	payload := make(map[string]any, len(evt.Payload)+1)
	for k, v := range evt.Payload {
		payload[k] = v
	}
	payload[memorySyncedKey] = true
	evt.Payload = payload
	return evt
}
