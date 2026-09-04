package store

import (
	"database/sql"
	"fmt"
	"time"
)

// HarnessSession is one conversation with a coding harness, as the supervisor
// remembers it across restarts.
type HarnessSession struct {
	Key              string
	HarnessSessionID string
	Kind             string
	Model            string
	PID              int
	State            string
	Workdir          string
	StartedAt        time.Time
	LastActivityAt   time.Time
	Turns            int
	CostUSD          float64
	InputTokens      int64
	OutputTokens     int64
	CacheRead        int64
	LastError        string
}

// Session states. A session is live only while a process is actually running;
// everything else is a claim about the past.
const (
	HarnessStarting = "starting"
	HarnessLive     = "live"
	HarnessIdle     = "idle"
	HarnessDead     = "dead"
	HarnessClosed   = "closed"
)

// SaveHarnessSession records a session before its process exists.
//
// Written first, deliberately. A row without a process is reapable; a process
// without a row is an orphan nothing can find.
func (s *Store) SaveHarnessSession(h HarnessSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.exec(`
		INSERT INTO harness_sessions
			(key, harness_session_id, kind, model, pid, state, workdir,
			 started_at, last_activity_at, turns, cost_usd, input_tokens,
			 output_tokens, cache_read, last_error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET
			harness_session_id = excluded.harness_session_id,
			kind = excluded.kind, model = excluded.model, pid = excluded.pid,
			state = excluded.state, workdir = excluded.workdir,
			last_activity_at = excluded.last_activity_at,
			last_error = excluded.last_error`,
		h.Key, h.HarnessSessionID, h.Kind, h.Model, h.PID, h.State, h.Workdir,
		h.StartedAt, h.LastActivityAt, h.Turns, h.CostUSD, h.InputTokens,
		h.OutputTokens, h.CacheRead, h.LastError)
	if err != nil {
		return fmt.Errorf("save harness session: %w", err)
	}
	return nil
}

// RecordHarnessTurn adds one turn's usage to a session's running totals.
//
// Accumulated in SQL rather than read-modify-written in Go: two turns finishing
// close together would otherwise each add to the same stale total and one of
// them would vanish from the bill.
func (s *Store) RecordHarnessTurn(key string, costUSD float64, in, out, cacheRead int64, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.exec(`
		UPDATE harness_sessions SET
			turns = turns + 1,
			cost_usd = cost_usd + ?,
			input_tokens = input_tokens + ?,
			output_tokens = output_tokens + ?,
			cache_read = cache_read + ?,
			last_activity_at = ?
		WHERE key = ?`, costUSD, in, out, cacheRead, at, key)
	if err != nil {
		return fmt.Errorf("record harness turn: %w", err)
	}
	return nil
}

// SetHarnessState moves a session between states, recording why when it died.
func (s *Store) SetHarnessState(key, state, lastErr string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.exec(`UPDATE harness_sessions SET state = ?, last_error = ?, last_activity_at = ? WHERE key = ?`,
		state, lastErr, at, key)
	if err != nil {
		return fmt.Errorf("set harness state: %w", err)
	}
	return nil
}

// GetHarnessSession returns one session, or nil when the key is unknown.
func (s *Store) GetHarnessSession(key string) (*HarnessSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.queryRow(`
		SELECT key, harness_session_id, kind, model, pid, state, workdir,
		       started_at, last_activity_at, turns, cost_usd, input_tokens,
		       output_tokens, cache_read, last_error
		FROM harness_sessions WHERE key = ?`, key)

	var h HarnessSession
	err := row.Scan(&h.Key, &h.HarnessSessionID, &h.Kind, &h.Model, &h.PID, &h.State,
		&h.Workdir, &h.StartedAt, &h.LastActivityAt, &h.Turns, &h.CostUSD,
		&h.InputTokens, &h.OutputTokens, &h.CacheRead, &h.LastError)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get harness session: %w", err)
	}
	return &h, nil
}

// ListHarnessSessions returns sessions in the given states, newest activity
// first. No states means all of them.
func (s *Store) ListHarnessSessions(states ...string) ([]HarnessSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := `SELECT key, harness_session_id, kind, model, pid, state, workdir,
	             started_at, last_activity_at, turns, cost_usd, input_tokens,
	             output_tokens, cache_read, last_error
	      FROM harness_sessions`
	args := make([]any, 0, len(states))
	if len(states) > 0 {
		q += " WHERE state IN (?" + repeatPlaceholders(len(states)-1) + ")"
		for _, st := range states {
			args = append(args, st)
		}
	}
	q += " ORDER BY last_activity_at DESC"

	rows, err := s.query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list harness sessions: %w", err)
	}
	defer rows.Close()

	var out []HarnessSession
	for rows.Next() {
		var h HarnessSession
		if err := rows.Scan(&h.Key, &h.HarnessSessionID, &h.Kind, &h.Model, &h.PID,
			&h.State, &h.Workdir, &h.StartedAt, &h.LastActivityAt, &h.Turns,
			&h.CostUSD, &h.InputTokens, &h.OutputTokens, &h.CacheRead, &h.LastError); err != nil {
			return nil, fmt.Errorf("scan harness session: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// DeleteHarnessSession forgets a session entirely, for ephemeral kinds whose
// transcript is discarded on close.
func (s *Store) DeleteHarnessSession(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`DELETE FROM harness_sessions WHERE key = ?`, key)
	return err
}

func repeatPlaceholders(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += ",?"
	}
	return out
}
