package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The durable event log: append-only, ordered by seq, partitioned by workspace.

// DefaultWorkspace is the partition a single-user instance writes to. The column
// exists now because adding one to a live log later means rewriting every offset.
const DefaultWorkspace = "default"

// LogEvent is one appended event. Seq is the only ordering that matters —
// timestamps come from wall clocks and can go backwards.
type LogEvent struct {
	Seq       int64
	ID        string
	Workspace string
	Kind      string
	AgentID   string
	Payload   map[string]any
	Meta      map[string]string
	CreatedAt time.Time
}

// DeadLetter is an event a subscriber could not process and has given up on.
type DeadLetter struct {
	ID         int64
	Subscriber string
	EventSeq   int64
	EventID    string
	Kind       string
	Attempts   int
	LastError  string
	CreatedAt  time.Time
}

// AppendLogEvent appends one event and returns its seq. A duplicate id returns
// the first one's seq, which is what keeps republishing idempotent.
func (s *Store) AppendLogEvent(e LogEvent) (int64, error) {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return 0, fmt.Errorf("store: encode event payload: %w", err)
	}
	meta, err := json.Marshal(e.Meta)
	if err != nil {
		return 0, fmt.Errorf("store: encode event meta: %w", err)
	}
	if e.Workspace == "" {
		e.Workspace = DefaultWorkspace
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	seq, inserted, err := s.insertReturningID(`
INSERT INTO event_log (event_id, workspace, kind, agent_id, payload, meta, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(event_id) DO NOTHING`, "seq",
		e.ID, e.Workspace, e.Kind, e.AgentID, string(payload), string(meta), e.CreatedAt)
	if err != nil {
		return 0, err
	}
	if inserted {
		return seq, nil
	}
	// Already logged: a redelivery must return the sequence it got the first
	// time, not a new one.
	err = s.queryRow(`SELECT seq FROM event_log WHERE event_id = ?`, e.ID).Scan(&seq)
	return seq, err
}

// LogEventsAfter returns up to limit events past seq, oldest first. An empty
// kinds slice matches every kind.
func (s *Store) LogEventsAfter(workspace string, after int64, kinds []string, limit int) ([]LogEvent, error) {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	if limit <= 0 {
		limit = 128
	}

	query := `SELECT seq, event_id, workspace, kind, agent_id, payload, meta, created_at
FROM event_log WHERE workspace = ? AND seq > ?`
	args := []any{workspace, after}
	if len(kinds) > 0 {
		query += ` AND kind IN (?` + strings.Repeat(`, ?`, len(kinds)-1) + `)`
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	query += ` ORDER BY seq ASC LIMIT ?` // a log replayed out of order is not a log
	args = append(args, limit)

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogEvent
	for rows.Next() {
		e, err := scanLogEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecentLogEvents returns the newest events first, for the operator's view.
func (s *Store) RecentLogEvents(workspace string, kindFilter string, limit int) ([]LogEvent, error) {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT seq, event_id, workspace, kind, agent_id, payload, meta, created_at
FROM event_log WHERE workspace = ?`
	args := []any{workspace}
	if kindFilter != "" {
		query += ` AND kind = ?`
		args = append(args, kindFilter)
	}
	query += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogEvent
	for rows.Next() {
		e, err := scanLogEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LogEventsByAgent returns the newest events attributed to one agent.
func (s *Store) LogEventsByAgent(workspace, agentID string, limit int) ([]LogEvent, error) {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	if limit <= 0 {
		limit = 50
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT seq, event_id, workspace, kind, agent_id, payload, meta, created_at
FROM event_log WHERE workspace = ? AND agent_id = ? ORDER BY seq DESC LIMIT ?`,
		workspace, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogEvent
	for rows.Next() {
		e, err := scanLogEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanLogEvent(rows *sql.Rows) (LogEvent, error) {
	var (
		e                 LogEvent
		agentID           sql.NullString
		payload, metaJSON string
	)
	if err := rows.Scan(&e.Seq, &e.ID, &e.Workspace, &e.Kind, &agentID,
		&payload, &metaJSON, &e.CreatedAt); err != nil {
		return e, err
	}
	e.AgentID = agentID.String
	// A payload that will not decode must not wedge the subscriber behind it.
	_ = json.Unmarshal([]byte(payload), &e.Payload)
	_ = json.Unmarshal([]byte(metaJSON), &e.Meta)
	return e, nil
}

// ConsumerOffset is how far a subscriber has read, and whether it has ever run.
//
// The two are different and conflating them is expensive: a subscriber that has
// never run must not be treated as one sitting at zero, or adding a subscriber
// replays the entire history through it.
func (s *Store) ConsumerOffset(name, workspace string) (seq int64, known bool, err error) {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	err = s.queryRow(
		`SELECT seq FROM event_offsets WHERE subscriber = ? AND workspace = ?`,
		name, workspace).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return seq, err == nil, err
}

// LogHead is the newest sequence number, where a fresh subscriber starts.
func (s *Store) LogHead(workspace string) (int64, error) {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var seq sql.NullInt64
	err := s.queryRow(`SELECT MAX(seq) FROM event_log WHERE workspace = ?`, workspace).Scan(&seq)
	return seq.Int64, err
}

// SetConsumerOffset records progress. Never moves backwards, or two workers
// sharing a name would undo each other and redeliver forever.
func (s *Store) SetConsumerOffset(name, workspace string, seq int64) error {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.exec(`
INSERT INTO event_offsets (subscriber, workspace, seq, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT(subscriber, workspace) DO UPDATE SET
  seq = MAX(event_offsets.seq, excluded.seq),
  updated_at = excluded.updated_at`,
		name, workspace, seq, time.Now())
	return err
}

// SafeSeq is the lowest offset any subscriber has reached: the prune limit.
func (s *Store) SafeSeq(workspace string) (int64, error) {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var seq sql.NullInt64
	err := s.queryRow(
		`SELECT MIN(seq) FROM event_offsets WHERE workspace = ?`, workspace).Scan(&seq)
	if err != nil {
		return 0, err
	}
	return seq.Int64, nil
}

// RecordDeadLetter files an event a subscriber gave up on.
func (s *Store) RecordDeadLetter(d DeadLetter) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.exec(`
INSERT INTO event_dead_letters (subscriber, event_seq, event_id, kind, attempts, last_error, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.Subscriber, d.EventSeq, d.EventID, d.Kind, d.Attempts, d.LastError, time.Now())
	return err
}

// DeadLetters returns the most recent give-ups, newest first.
func (s *Store) DeadLetters(limit int) ([]DeadLetter, error) {
	if limit <= 0 {
		limit = 50
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT id, subscriber, event_seq, event_id, kind, attempts, last_error, created_at
FROM event_dead_letters ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeadLetter
	for rows.Next() {
		var d DeadLetter
		if err := rows.Scan(&d.ID, &d.Subscriber, &d.EventSeq, &d.EventID,
			&d.Kind, &d.Attempts, &d.LastError, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PruneEventLog drops events older than before that EVERY subscriber has read.
// Pruning by age alone would delete work a lagging subscriber had not done.
func (s *Store) PruneEventLog(workspace string, before time.Time) (int64, error) {
	safe, err := s.SafeSeq(workspace)
	if err != nil {
		return 0, err
	}
	if workspace == "" {
		workspace = DefaultWorkspace
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.exec(
		`DELETE FROM event_log WHERE workspace = ? AND created_at < ? AND seq <= ?`,
		workspace, before, safe)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
