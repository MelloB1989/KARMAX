package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The durable event log. Append-only, ordered by seq, partitioned by workspace.
// Subscribers record how far they have read, so a restart resumes rather than
// starting blind.

// DefaultWorkspace is the partition a single-user instance writes to. The
// column exists now because adding a partition key to a live log later means
// rewriting every offset, and one instance per person is only the current
// shape, not a permanent one.
const DefaultWorkspace = "default"

// LogEvent is one appended event. Seq is assigned on append and is the only
// ordering that matters — timestamps come from wall clocks and can go backwards.
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

// AppendLogEvent appends one event and returns its sequence number.
//
// A duplicate id is not an error: publishing is retried in places, and the
// second append returning the first one's seq keeps that idempotent.
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

	res, err := s.db.Exec(`
INSERT INTO event_log (event_id, workspace, kind, agent_id, payload, meta, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(event_id) DO NOTHING`,
		e.ID, e.Workspace, e.Kind, e.AgentID, string(payload), string(meta), e.CreatedAt)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return res.LastInsertId()
	}
	var seq int64
	err = s.db.QueryRow(`SELECT seq FROM event_log WHERE event_id = ?`, e.ID).Scan(&seq)
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
	// Ascending: a log replayed out of order is not a log.
	query += ` ORDER BY seq ASC LIMIT ?`
	args = append(args, limit)

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(query, args...)
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

	rows, err := s.db.Query(query, args...)
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

	rows, err := s.db.Query(`
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
	// A payload that will not decode must not stop the whole read: the event
	// still happened, and a consumer seeing it empty is better than a
	// subscriber that cannot advance past it.
	_ = json.Unmarshal([]byte(payload), &e.Payload)
	_ = json.Unmarshal([]byte(metaJSON), &e.Meta)
	return e, nil
}

// ConsumerOffset is how far a subscriber has read. Zero for one that has never
// run, which means it starts from the beginning of what is retained.
func (s *Store) ConsumerOffset(name, workspace string) (int64, error) {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var seq int64
	err := s.db.QueryRow(
		`SELECT seq FROM event_offsets WHERE subscriber = ? AND workspace = ?`,
		name, workspace).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return seq, err
}

// SetConsumerOffset records that a subscriber has finished everything up to seq.
//
// Never moves backwards. Two workers for one subscriber name would otherwise
// undo each other's progress and redeliver indefinitely.
func (s *Store) SetConsumerOffset(name, workspace string, seq int64) error {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
INSERT INTO event_offsets (subscriber, workspace, seq, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT(subscriber, workspace) DO UPDATE SET
  seq = MAX(event_offsets.seq, excluded.seq),
  updated_at = excluded.updated_at`,
		name, workspace, seq, time.Now())
	return err
}

// SafeSeq is the lowest offset any subscriber has reached — the point past
// which nothing may be pruned, because somebody has not read it yet.
func (s *Store) SafeSeq(workspace string) (int64, error) {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var seq sql.NullInt64
	err := s.db.QueryRow(
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

	_, err := s.db.Exec(`
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

	rows, err := s.db.Query(`
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

// PruneEventLog drops events older than before that every subscriber has
// already read. The second condition is the one that matters: pruning by age
// alone would silently delete work a lagging subscriber had not done yet.
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

	res, err := s.db.Exec(
		`DELETE FROM event_log WHERE workspace = ? AND created_at < ? AND seq <= ?`,
		workspace, before, safe)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
