package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Durable timers. A timer set here fires after a restart, a crash, or a week of
// downtime — which is the difference between "wait three days, then continue"
// and "run again on a cron and work out where you were".

// Timer is one scheduled future event.
type Timer struct {
	// ID is chosen by the caller so that re-arming is idempotent: setting the
	// same id twice moves the deadline rather than firing twice.
	ID        string
	Workspace string
	FireAt    time.Time
	// Kind is the event kind published when it fires.
	Kind    string
	AgentID string
	// Loop names the loop to re-trigger, when a loop set this timer.
	Loop      string
	Payload   map[string]any
	CreatedAt time.Time
	FiredAt   *time.Time
}

// SetTimer arms a timer, replacing any pending one with the same id.
func (s *Store) SetTimer(t Timer) error {
	if strings.TrimSpace(t.ID) == "" {
		return fmt.Errorf("store: a timer needs an id")
	}
	payload, err := json.Marshal(t.Payload)
	if err != nil {
		return fmt.Errorf("store: encode timer payload: %w", err)
	}
	if t.Workspace == "" {
		t.Workspace = DefaultWorkspace
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// fired_at is reset on conflict: re-arming a timer that already went off is
	// how a loop schedules the next occurrence under a stable id.
	_, err = s.db.Exec(`
INSERT INTO timers (id, workspace, fire_at, kind, agent_id, loop, payload, created_at, fired_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
ON CONFLICT(id) DO UPDATE SET
  fire_at = excluded.fire_at,
  kind = excluded.kind,
  agent_id = excluded.agent_id,
  loop = excluded.loop,
  payload = excluded.payload,
  fired_at = NULL`,
		t.ID, t.Workspace, t.FireAt, t.Kind, t.AgentID, t.Loop, string(payload), t.CreatedAt)
	return err
}

// DueTimers returns armed timers whose deadline has passed, earliest first.
func (s *Store) DueTimers(workspace string, now time.Time, limit int) ([]Timer, error) {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	if limit <= 0 {
		limit = 64
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
SELECT id, workspace, fire_at, kind, agent_id, loop, payload, created_at, fired_at
FROM timers WHERE workspace = ? AND fired_at IS NULL AND fire_at <= ?
ORDER BY fire_at ASC LIMIT ?`, workspace, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Timer
	for rows.Next() {
		t, err := scanTimer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PendingTimers returns armed timers that have not fired, earliest first.
func (s *Store) PendingTimers(workspace string, limit int) ([]Timer, error) {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	if limit <= 0 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
SELECT id, workspace, fire_at, kind, agent_id, loop, payload, created_at, fired_at
FROM timers WHERE workspace = ? AND fired_at IS NULL ORDER BY fire_at ASC LIMIT ?`,
		workspace, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Timer
	for rows.Next() {
		t, err := scanTimer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TimerByID returns one timer, armed or already fired.
func (s *Store) TimerByID(id string) (*Timer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
SELECT id, workspace, fire_at, kind, agent_id, loop, payload, created_at, fired_at
FROM timers WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	t, err := scanTimer(rows)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// MarkTimerFired records that a timer went off. The fired_at guard makes a
// second attempt a no-op, so a retry after a crash cannot double-fire.
func (s *Store) MarkTimerFired(id string, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(
		`UPDATE timers SET fired_at = ? WHERE id = ? AND fired_at IS NULL`, at, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CancelTimer disarms a timer. Cancelling one that does not exist is not an
// error: the caller wanted it gone, and it is.
func (s *Store) CancelTimer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM timers WHERE id = ?`, id)
	return err
}

// CancelLoopTimers disarms every pending timer a loop set, used when the loop
// is uninstalled or disabled so it cannot be woken by its own past.
func (s *Store) CancelLoopTimers(loop string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM timers WHERE loop = ? AND fired_at IS NULL`, loop)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneTimers drops fired timers older than before. Armed ones are never
// pruned, however old — a timer set for next year is not stale, it is waiting.
func (s *Store) PruneTimers(before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`DELETE FROM timers WHERE fired_at IS NOT NULL AND fired_at < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanTimer(rows *sql.Rows) (Timer, error) {
	var (
		t                      Timer
		agentID, loop, payload sql.NullString
		firedAt                sql.NullTime
	)
	if err := rows.Scan(&t.ID, &t.Workspace, &t.FireAt, &t.Kind, &agentID,
		&loop, &payload, &t.CreatedAt, &firedAt); err != nil {
		return t, err
	}
	t.AgentID, t.Loop = agentID.String, loop.String
	_ = json.Unmarshal([]byte(payload.String), &t.Payload)
	if firedAt.Valid {
		at := firedAt.Time
		t.FiredAt = &at
	}
	return t, nil
}
