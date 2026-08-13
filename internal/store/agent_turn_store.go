package store

import (
	"database/sql"
	"time"
)

// Durable state for agent turns.
//
// The bus is at-least-once and resumes each subscriber from its own offset, but
// the agent router acknowledged an event the moment it reached the mailbox —
// an in-memory channel — not when the turn finished. So a crash mid-turn lost
// both the work and the event: the offset had already advanced past it and it
// was never redelivered.
//
// Loops solved this years earlier with loop_runs plus a lease. This is the same
// shape for turns: the row is written before the work starts, finished when it
// ends, and anything left running when the process died is reaped on the next
// boot and retried.

// TurnStatus values.
const (
	TurnRunning     = "running"
	TurnOK          = "ok"
	TurnFailed      = "failed"
	TurnInterrupted = "interrupted"
	TurnDead        = "dead"
)

// AgentTurn is one attempt at handling one event.
type AgentTurn struct {
	ID         string
	AgentID    string
	EventID    string
	EventKind  string
	EventJSON  string
	Status     string
	Attempt    int
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      string
}

// StartAgentTurn records a turn as running.
//
// Conditional on the event, not the turn id: an event redelivered after a crash
// must resume the existing row rather than open a second turn beside it. The
// returned bool reports whether this caller owns the turn — false means another
// attempt is already in flight and this delivery is a duplicate.
func (s *Store) StartAgentTurn(t AgentTurn) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if t.StartedAt.IsZero() {
		t.StartedAt = time.Now()
	}
	res, err := s.db.Exec(`
INSERT INTO agent_turns (id, agent_id, event_id, event_kind, event_json, status, attempt, started_at)
VALUES (?, ?, ?, ?, ?, 'running', ?, ?)
ON CONFLICT(event_id) DO UPDATE SET
	status      = 'running',
	attempt     = agent_turns.attempt + 1,
	started_at  = excluded.started_at,
	-- Cleared with the rest: a retry that kept the previous attempt's
	-- finish time left the row reading as both running and finished, with
	-- finished_at earlier than started_at. Anything deciding "did this
	-- complete" from that timestamp got the wrong answer.
	finished_at = NULL,
	error       = ''
WHERE agent_turns.status IN ('interrupted', 'failed')`,
		t.ID, t.AgentID, t.EventID, t.EventKind, t.EventJSON, t.Attempt, t.StartedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// FinishAgentTurn closes a turn.
func (s *Store) FinishAgentTurn(eventID, status, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	_, err := s.db.Exec(`
UPDATE agent_turns SET status = ?, finished_at = ?, error = ? WHERE event_id = ?`,
		status, now, errMsg, eventID)
	return err
}

// ReapStaleTurns marks turns left running past a cutoff as interrupted and
// returns them, newest first. This is what a restart calls: anything still
// "running" when the process is starting up cannot be running.
func (s *Store) ReapStaleTurns(olderThan time.Time, maxAttempts int) ([]AgentTurn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
SELECT id, agent_id, event_id, event_kind, event_json, attempt
FROM agent_turns
WHERE status = 'running' AND started_at < ?
ORDER BY started_at DESC`, olderThan)
	if err != nil {
		return nil, err
	}
	var out []AgentTurn
	for rows.Next() {
		var t AgentTurn
		if err := rows.Scan(&t.ID, &t.AgentID, &t.EventID, &t.EventKind, &t.EventJSON, &t.Attempt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// A turn that has already burned its attempts is dead, not retryable —
	// otherwise a poisonous event resurrects itself on every boot forever.
	for i := range out {
		status := TurnInterrupted
		if out[i].Attempt >= maxAttempts {
			status = TurnDead
		}
		if _, err := s.db.Exec(`UPDATE agent_turns SET status = ?, finished_at = ? WHERE event_id = ?`,
			status, time.Now(), out[i].EventID); err != nil {
			return nil, err
		}
		out[i].Status = status
	}
	return out, nil
}

// RecentTurns returns the most recent turns for an agent.
func (s *Store) RecentTurns(agentID string, limit int) ([]AgentTurn, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
SELECT id, agent_id, event_id, event_kind, status, attempt, started_at, finished_at, error
FROM agent_turns WHERE agent_id = ? ORDER BY started_at DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AgentTurn
	for rows.Next() {
		var t AgentTurn
		var fin sql.NullTime
		if err := rows.Scan(&t.ID, &t.AgentID, &t.EventID, &t.EventKind, &t.Status,
			&t.Attempt, &t.StartedAt, &fin, &t.Error); err != nil {
			return nil, err
		}
		if fin.Valid {
			t.FinishedAt = &fin.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PruneAgentTurns drops finished turns older than a cutoff, keeping dead ones
// so a permanent failure stays visible.
func (s *Store) PruneAgentTurns(before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
DELETE FROM agent_turns WHERE started_at < ? AND status NOT IN ('running', 'dead')`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
