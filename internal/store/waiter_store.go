package store

import (
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// A run parked on something that has not happened yet. The row is what makes
// "wait until the ticket is prioritised" survive a restart: the run ends, and
// the event that matches revives it days later.

// Waiter is one run suspended on an external event.
type Waiter struct {
	ID, ExecutionID, Loop, Step, CaseID, EventKind, MatchJSON string
	ExpiresAt                                                 *time.Time
	CreatedAt                                                 time.Time
	ResolvedAt                                                *time.Time
	ResultJSON                                                string
}

const waiterColumns = `id, execution_id, loop, step, case_id, event_kind, match_json, expires_at, created_at, resolved_at, result_json`

func scanWaiter(row rowScanner) (Waiter, error) {
	var w Waiter
	var expiresAt, resolvedAt sql.NullTime
	if err := row.Scan(&w.ID, &w.ExecutionID, &w.Loop, &w.Step, &w.CaseID, &w.EventKind, &w.MatchJSON,
		&expiresAt, &w.CreatedAt, &resolvedAt, &w.ResultJSON); err != nil {
		return Waiter{}, err
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		w.ExpiresAt = &t
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time
		w.ResolvedAt = &t
	}
	return w, nil
}

// ArmWaiter records a run's suspension. It is idempotent on (execution_id,
// step) — the unique index a redelivered await hits — so a step re-entering
// Await after redelivery rejoins the waiter it already armed rather than
// duplicating or resetting it.
func (s *Store) ArmWaiter(w Waiter) error {
	if w.ID == "" {
		w.ID = uuid.New().String()
	}
	if w.MatchJSON == "" {
		w.MatchJSON = "{}"
	}
	if w.CreatedAt.IsZero() {
		w.CreatedAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO waiters (id, execution_id, loop, step, case_id, event_kind, match_json, expires_at, created_at, resolved_at, result_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '')
ON CONFLICT(execution_id, step) DO NOTHING`,
		w.ID, w.ExecutionID, w.Loop, w.Step, w.CaseID, w.EventKind, w.MatchJSON, w.ExpiresAt, w.CreatedAt)
	return err
}

// PendingWaiters returns unresolved, unexpired waiters for one event kind —
// the candidates an incoming event of that kind is matched against.
func (s *Store) PendingWaiters(eventKind string) ([]Waiter, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT `+waiterColumns+`
FROM waiters
WHERE event_kind = ? AND resolved_at IS NULL AND (expires_at IS NULL OR expires_at > ?)
ORDER BY created_at ASC`, eventKind, time.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Waiter
	for rows.Next() {
		w, err := scanWaiter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ResolveWaiter is a compare-and-set: it only ever resolves a waiter once, so
// two events matching the same waiter concurrently cannot both resume the run.
// The single UPDATE...WHERE is what makes that atomic — read-then-write would
// let both callers see "unresolved".
func (s *Store) ResolveWaiter(id, resultJSON string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.exec(`
UPDATE waiters SET resolved_at = ?, result_json = ? WHERE id = ? AND resolved_at IS NULL`,
		time.Now(), resultJSON, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// WaiterResult returns a step's result once its waiter has resolved. The bool
// is false both when the waiter does not exist yet and when it is still
// pending — Await treats both the same way: keep waiting.
func (s *Store) WaiterResult(executionID, step string) (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var resultJSON string
	var resolvedAt sql.NullTime
	err := s.queryRow(`
SELECT result_json, resolved_at FROM waiters WHERE execution_id = ? AND step = ?`,
		executionID, step).Scan(&resultJSON, &resolvedAt)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !resolvedAt.Valid {
		return "", false, nil
	}
	return resultJSON, true, nil
}

// ExpireWaiters resolves waiters past their deadline with a timeout result and
// returns exactly the ones it just expired. The UPDATE is guarded by
// resolved_at IS NULL, the same compare-and-set as ResolveWaiter, so a waiter
// already expired or resolved by another caller is not counted twice.
func (s *Store) ExpireWaiters(now time.Time) ([]Waiter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.query(`
SELECT `+waiterColumns+`
FROM waiters WHERE resolved_at IS NULL AND expires_at IS NOT NULL AND expires_at <= ?`, now)
	if err != nil {
		return nil, err
	}
	var out []Waiter
	for rows.Next() {
		w, err := scanWaiter(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}

	const timeoutResult = `{"timeout":true}`
	if _, err := s.exec(`
UPDATE waiters SET resolved_at = ?, result_json = ?
WHERE resolved_at IS NULL AND expires_at IS NOT NULL AND expires_at <= ?`,
		now, timeoutResult, now); err != nil {
		return nil, err
	}
	for i := range out {
		t := now
		out[i].ResolvedAt = &t
		out[i].ResultJSON = timeoutResult
	}
	return out, nil
}
