package store

import (
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"
)

// A case is the thread of work an agent is on: one ticket, one onboarding, one
// incident. Without it the workflows that make up an agent are unrelated runs
// days apart, and the operator sees five robots rather than one colleague.

// Case is one thread of work.
type Case struct {
	ID, Org, Agent, Key, Title, State, Namespace string
	ThreadChannel, ThreadTS                      string
	CreatedAt, UpdatedAt                         time.Time
}

// CaseEvent is one entry in a case's log: a note, a status change, a message.
type CaseEvent struct {
	ID, CaseID, Kind, Payload, Actor string
	CreatedAt                        time.Time
}

const caseColumns = `id, org, agent, ckey, title, state, namespace, thread_channel, thread_ts, created_at, updated_at`

func scanCase(row rowScanner) (Case, error) {
	var c Case
	if err := row.Scan(&c.ID, &c.Org, &c.Agent, &c.Key, &c.Title, &c.State, &c.Namespace,
		&c.ThreadChannel, &c.ThreadTS, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Case{}, err
	}
	return c, nil
}

// OpenCase upserts on Key and is idempotent: a redelivered webhook must rejoin
// the existing case with its existing id and state rather than reset either, so
// the conflict branch does nothing and the row is always read back fresh.
func (s *Store) OpenCase(c Case) (Case, error) {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	if c.State == "" {
		c.State = "open"
	}
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = now
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.exec(`
INSERT INTO cases (id, org, agent, ckey, title, state, namespace, thread_channel, thread_ts, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(ckey) DO NOTHING`,
		c.ID, c.Org, c.Agent, c.Key, c.Title, c.State, c.Namespace, c.ThreadChannel, c.ThreadTS, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return Case{}, err
	}

	return scanCase(s.queryRow(`SELECT `+caseColumns+` FROM cases WHERE ckey = ?`, c.Key))
}

// CaseByKey looks a case up by its idempotency key.
func (s *Store) CaseByKey(key string) (Case, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, err := scanCase(s.queryRow(`SELECT `+caseColumns+` FROM cases WHERE ckey = ?`, key))
	if err == sql.ErrNoRows {
		return Case{}, false, nil
	}
	if err != nil {
		return Case{}, false, err
	}
	return c, true, nil
}

// CaseByID looks a case up by its own id.
func (s *Store) CaseByID(id string) (Case, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, err := scanCase(s.queryRow(`SELECT `+caseColumns+` FROM cases WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return Case{}, false, nil
	}
	if err != nil {
		return Case{}, false, err
	}
	return c, true, nil
}

// SetCaseState moves a case to a new state.
func (s *Store) SetCaseState(id, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`UPDATE cases SET state = ?, updated_at = datetime('now') WHERE id = ?`, state, id)
	return err
}

// BindCaseThread records the chat thread a case lives in, so later replies land
// in the same place.
func (s *Store) BindCaseThread(id, channel, ts string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
UPDATE cases SET thread_channel = ?, thread_ts = ?, updated_at = datetime('now') WHERE id = ?`,
		channel, ts, id)
	return err
}

// ListCases returns cases for an agent and/or state, most recently updated
// first. Either filter is skipped when empty.
func (s *Store) ListCases(agent, state string, limit int) ([]Case, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := `SELECT ` + caseColumns + ` FROM cases`
	var conds []string
	var args []any
	if agent != "" {
		conds = append(conds, "agent = ?")
		args = append(args, agent)
	}
	if state != "" {
		conds = append(conds, "state = ?")
		args = append(args, state)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Case
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AppendCaseEvent files one entry in a case's log.
func (s *Store) AppendCaseEvent(e CaseEvent) error {
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO case_events (id, case_id, kind, payload, actor, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
		e.ID, e.CaseID, e.Kind, e.Payload, e.Actor, e.CreatedAt)
	return err
}

// CaseHistory returns a case's most recent log entries, newest first.
func (s *Store) CaseHistory(caseID string, limit int) ([]CaseEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT id, case_id, kind, payload, actor, created_at
FROM case_events WHERE case_id = ? ORDER BY created_at DESC LIMIT ?`, caseID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CaseEvent
	for rows.Next() {
		var e CaseEvent
		if err := rows.Scan(&e.ID, &e.CaseID, &e.Kind, &e.Payload, &e.Actor, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
