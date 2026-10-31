package store

import (
	"database/sql"
	"strings"
	"time"
)

// Loop triggers: the event a loop was woken by, owned until its run ends.
//
// The loop subscriber used to start the run in a goroutine and return, so the
// bus moved past the event at once; a busy loop queued it in memory. A crash
// in the run — or while it waited, or in the second or two before a workflow
// handed its message to the full agent — lost the message for good, and a
// resumed run came back with no trigger at all. Now the event is a row before
// the subscriber returns, and stays open until its run is over.

// Loop trigger states.
const (
	LoopTriggerPending = "pending" // recorded, not started
	LoopTriggerQueued  = "queued"  // waiting for the loop to be free
	LoopTriggerRunning = "running" // a run is working on it
	LoopTriggerRetry   = "retry"   // its run failed; the loop's retry replays it
	LoopTriggerDone    = "done"    // its run ended, or nothing is left to do
)

// LoopTrigger is one event one loop was triggered by.
type LoopTrigger struct {
	Key, Loop, EventID, Conversation, Kind, Payload string
	Status                                          string
	// Owner is the request the run handed the message to (a workflow's ask):
	// while that request exists, it — not a replay — answers the message.
	Owner string
	// Acted: the run already sent, called or posted something. A replay
	// would re-compose and send again, so it is not replayed.
	Acted                bool
	Attempts             int
	CreatedAt, UpdatedAt time.Time
}

// LoopTriggerKey is a trigger's row key: one per loop per event. (Not a NUL
// separator: Postgres refuses NUL in text.)
func LoopTriggerKey(loop, eventID string) string { return loop + "|" + eventID }

const loopTriggerColumns = `trigger_key, loop_name, event_id, conversation, kind, payload, status, owner,
	acted, attempts, created_at, updated_at`

func scanLoopTrigger(r rowScanner) (LoopTrigger, error) {
	var t LoopTrigger
	var acted int
	err := r.Scan(&t.Key, &t.Loop, &t.EventID, &t.Conversation, &t.Kind, &t.Payload, &t.Status, &t.Owner,
		&acted, &t.Attempts, &t.CreatedAt, &t.UpdatedAt)
	t.Acted = acted != 0
	return t, err
}

// RecordLoopTrigger writes a trigger as pending. created is false when the
// event was already recorded for that loop — a redelivery.
func (s *Store) RecordLoopTrigger(t LoopTrigger) (created bool, err error) {
	if t.Key == "" {
		t.Key = LoopTriggerKey(t.Loop, t.EventID)
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.exec(`
INSERT OR IGNORE INTO loop_triggers (`+loopTriggerColumns+`)
VALUES (?, ?, ?, ?, ?, ?, 'pending', '', 0, 0, ?, ?)`,
		t.Key, t.Loop, t.EventID, t.Conversation, t.Kind, t.Payload, now, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetLoopTriggerStatus moves a trigger along; attempt counts a run starting.
func (s *Store) SetLoopTriggerStatus(key, status string, attempt bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := `UPDATE loop_triggers SET status = ?, updated_at = ?`
	if attempt {
		q += `, attempts = attempts + 1`
	}
	_, err := s.exec(q+` WHERE trigger_key = ?`, status, time.Now(), key)
	return err
}

// MarkLoopTriggerActed records that the trigger's run has put something in
// front of another person.
func (s *Store) MarkLoopTriggerActed(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`UPDATE loop_triggers SET acted = 1, updated_at = ? WHERE trigger_key = ?`, time.Now(), key)
	return err
}

// SetLoopTriggerOwner records the request the trigger's message was handed to.
func (s *Store) SetLoopTriggerOwner(key, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`UPDATE loop_triggers SET owner = ?, updated_at = ? WHERE trigger_key = ?`, owner, time.Now(), key)
	return err
}

// GetLoopTrigger reads one trigger.
func (s *Store) GetLoopTrigger(key string) (LoopTrigger, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, err := scanLoopTrigger(s.queryRow(`SELECT `+loopTriggerColumns+` FROM loop_triggers WHERE trigger_key = ?`, key))
	if err == sql.ErrNoRows {
		return LoopTrigger{}, false, nil
	}
	if err != nil {
		return LoopTrigger{}, false, err
	}
	return t, true, nil
}

// OpenLoopTriggers are a loop's ("" for every loop) triggers not yet done,
// oldest first — limited to the given states when any are named.
func (s *Store) OpenLoopTriggers(loop string, statuses ...string) ([]LoopTrigger, error) {
	if len(statuses) == 0 {
		statuses = []string{LoopTriggerPending, LoopTriggerQueued, LoopTriggerRunning, LoopTriggerRetry}
	}
	q := `SELECT ` + loopTriggerColumns + ` FROM loop_triggers WHERE status IN (?` +
		strings.Repeat(", ?", len(statuses)-1) + `)`
	args := make([]any, 0, len(statuses)+1)
	for _, st := range statuses {
		args = append(args, st)
	}
	if loop != "" {
		q += ` AND loop_name = ?`
		args = append(args, loop)
	}
	q += ` ORDER BY created_at ASC`
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LoopTrigger
	for rows.Next() {
		t, err := scanLoopTrigger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SupersedeLoopTriggers closes a loop's waiting triggers in one conversation
// recorded at or before upTo: a pass over that conversation read its whole
// thread, so it answered them too.
func (s *Store) SupersedeLoopTriggers(loop, conversation string, upTo time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.exec(`
UPDATE loop_triggers SET status = 'done', updated_at = ?
WHERE loop_name = ? AND conversation = ? AND created_at <= ? AND status IN ('pending', 'queued', 'retry')`,
		time.Now(), loop, conversation, upTo)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneLoopTriggers drops finished triggers older than a cutoff.
func (s *Store) PruneLoopTriggers(before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.exec(`DELETE FROM loop_triggers WHERE status = 'done' AND updated_at < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
