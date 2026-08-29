package comms

import (
	"errors"
	"testing"
)

// Slack resends when an ack is slow, and an agent turn takes longer than Slack
// waits — so a redelivery is the ordinary case on any message that makes the
// agent think, not an error.
//
// The consequence of missing it is not a duplicate log line. It is the whole
// turn running twice: another model call billed, and for "spin up a sandbox and
// raise a PR" the work done twice. The reply de-duplicator downstream catches
// the second ANSWER, which is exactly what hides it — the operator sees one
// reply and never learns the side effects happened twice.
func TestARedeliveredMessageIsRecognised(t *testing.T) {
	// The three dialects the store can be speaking.
	for _, err := range []error{
		errors.New("save channel message: UNIQUE constraint failed: channel_messages.id"),
		errors.New(`pq: duplicate key value violates unique constraint "channel_messages_pkey"`),
		errors.New("Error 1062: Duplicate entry '123' for key 'PRIMARY'"),
	} {
		if !isDuplicateMessage(err) {
			t.Errorf("not recognised as a redelivery, so the turn would run twice: %v", err)
		}
	}
}

// A real failure must stay a failure: swallowing a disk error as "already seen"
// would drop the message silently.
func TestARealStoreFailureIsNotMistakenForARedelivery(t *testing.T) {
	for _, err := range []error{
		errors.New("database is locked"),
		errors.New("disk I/O error"),
		errors.New("no such table: channel_messages"),
		nil,
	} {
		if isDuplicateMessage(err) {
			t.Errorf("a real failure was treated as a redelivery: %v", err)
		}
	}
}
