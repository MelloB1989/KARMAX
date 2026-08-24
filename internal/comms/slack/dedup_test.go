package slack

import (
	"testing"
	"time"
)

// The first time an envelope id is seen it goes through; a redelivery of the
// same id does not.
func TestEnvelopeDedupCatchesARedelivery(t *testing.T) {
	d := newEnvelopeDedup()
	now := time.Now()
	if d.seenBefore("Ev1", now) {
		t.Fatal("first delivery must not be reported as a duplicate")
	}
	if !d.seenBefore("Ev1", now.Add(time.Second)) {
		t.Fatal("a redelivery of the same envelope id must be caught")
	}
}

// Two different envelopes never collide.
func TestEnvelopeDedupDistinguishesIDs(t *testing.T) {
	d := newEnvelopeDedup()
	now := time.Now()
	if d.seenBefore("Ev1", now) {
		t.Fatal("Ev1 should not be a duplicate on first sight")
	}
	if d.seenBefore("Ev2", now) {
		t.Fatal("Ev2 should not be a duplicate on first sight")
	}
}

// An empty id is never deduped — there is nothing to key on, and refusing to
// dedupe is safer than silently dropping a message.
func TestEnvelopeDedupNeverCollapsesEmptyIDs(t *testing.T) {
	d := newEnvelopeDedup()
	now := time.Now()
	if d.seenBefore("", now) {
		t.Fatal("an empty id must never be reported as a duplicate")
	}
	if d.seenBefore("", now) {
		t.Fatal("an empty id must never be reported as a duplicate, even the second time")
	}
}

// An id outside the remembered window is forgotten, so a connection that
// stays open for days does not grow this map without bound.
func TestEnvelopeDedupForgetsOldIDs(t *testing.T) {
	d := newEnvelopeDedup()
	start := time.Now()
	d.seenBefore("Ev1", start)
	later := start.Add(envelopeWindow + time.Minute)
	if d.seenBefore("Ev1", later) {
		t.Fatal("an id outside the dedup window should have been forgotten")
	}
}
