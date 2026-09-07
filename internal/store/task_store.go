package store

import (
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Work the operator handed over.
//
// An agent turn answers and ends; a task outlives it. This is the difference
// between "I'll look into it" and something that is still being worked on
// tomorrow morning after two restarts.

// Task states. Only Done and Failed are terminal; Blocked is waiting on a
// person and comes back the moment they answer.
const (
	TaskOpen    = "open"
	TaskWorking = "working"
	TaskBlocked = "blocked"
	TaskDone    = "done"
	TaskFailed  = "failed"
)

// Task is one piece of work owned until it is finished.
type Task struct {
	ID, AgentID, Title, Goal, Status string
	SessionKey, Workdir              string
	ChannelID, Target                string
	Progress, Reported, LastError    string
	Attempts                         int
	NextActionAt                     *time.Time
	CreatedAt, UpdatedAt             time.Time
}

const taskColumns = `id, agent_id, title, goal, status, session_key, workdir, channel_id, target,
	progress, reported, last_error, attempts, next_action_at, created_at, updated_at`

func scanTask(row rowScanner) (Task, error) {
	var t Task
	var next sql.NullTime
	if err := row.Scan(&t.ID, &t.AgentID, &t.Title, &t.Goal, &t.Status, &t.SessionKey,
		&t.Workdir, &t.ChannelID, &t.Target, &t.Progress, &t.Reported, &t.LastError,
		&t.Attempts, &next, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return Task{}, err
	}
	if next.Valid {
		n := next.Time
		t.NextActionAt = &n
	}
	return t, nil
}

// CreateTask records work to be owned. Due immediately unless told otherwise,
// because the common case is the operator asking for something now.
func (s *Store) CreateTask(t Task) (Task, error) {
	if t.ID == "" {
		t.ID = uuid.New().String()
	}
	if t.Status == "" {
		t.Status = TaskOpen
	}
	if strings.TrimSpace(t.Title) == "" {
		t.Title = firstLineOf(t.Goal, 120)
	}
	if t.SessionKey == "" {
		t.SessionKey = "task:" + t.ID
	}
	now := time.Now()
	t.CreatedAt, t.UpdatedAt = now, now

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO tasks (`+taskColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.AgentID, t.Title, t.Goal, t.Status, t.SessionKey, t.Workdir,
		t.ChannelID, t.Target, t.Progress, t.Reported, t.LastError,
		t.Attempts, t.NextActionAt, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return Task{}, err
	}
	return t, nil
}

// DueTasks returns work that should be picked up now, oldest first.
//
// Bounded by the caller: a hundred tasks coming due together must not become a
// hundred model calls in one tick.
func (s *Store) DueTasks(now time.Time, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 5
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT `+taskColumns+` FROM tasks
WHERE status IN ('open', 'working')
  AND (next_action_at IS NULL OR next_action_at <= ?)
ORDER BY created_at ASC LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTasks returns tasks, newest activity first. An empty status means all of
// them; "live" means everything not yet finished.
func (s *Store) ListTasks(status string, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 50
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := `SELECT ` + taskColumns + ` FROM tasks`
	var args []any
	switch {
	case status == "live":
		q += ` WHERE status IN ('open', 'working', 'blocked')`
	case status != "":
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTask reads one task.
func (s *Store) GetTask(id string) (Task, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, err := scanTask(s.queryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	return t, true, nil
}

// TaskUpdate is one round's outcome.
type TaskUpdate struct {
	Status       string
	Progress     string
	LastError    string
	NextActionAt *time.Time
	// Reported is what the operator has now been told. Left empty it keeps
	// whatever was there, so a silent round does not erase the record of the
	// last thing they heard.
	Reported string
	// BumpAttempt counts a round that ran, which is what stops a task that
	// cannot make progress from running forever.
	BumpAttempt bool
}

// UpdateTask records a round's outcome.
func (s *Store) UpdateTask(id string, u TaskUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sets := []string{"updated_at = ?"}
	args := []any{time.Now()}
	if u.Status != "" {
		sets = append(sets, "status = ?")
		args = append(args, u.Status)
	}
	if u.Progress != "" {
		sets = append(sets, "progress = ?")
		args = append(args, u.Progress)
	}
	if u.Reported != "" {
		sets = append(sets, "reported = ?")
		args = append(args, u.Reported)
	}
	// Written even when empty: a round that succeeded must clear the error the
	// previous one left, or a finished task carries a stale failure forever.
	sets = append(sets, "last_error = ?")
	args = append(args, u.LastError)

	sets = append(sets, "next_action_at = ?")
	args = append(args, u.NextActionAt)

	if u.BumpAttempt {
		sets = append(sets, "attempts = attempts + 1")
	}
	args = append(args, id)

	_, err := s.exec(`UPDATE tasks SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	return err
}

// ReleaseStuckTasks brings back tasks left mid-round when the daemon died.
//
// A task marked working with its next action already long past is not being
// worked on — the process that was working on it is gone. Without this it sits
// there looking busy and is never picked up again.
func (s *Store) ReleaseStuckTasks(stuckSince time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.exec(`
UPDATE tasks SET next_action_at = NULL, updated_at = ?
WHERE status = 'working' AND updated_at < ?`, time.Now(), stuckSince)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneTasks drops finished tasks older than a cutoff. Failed ones survive, so
// something that never got done stays visible rather than ageing quietly out.
func (s *Store) PruneTasks(before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.exec(`DELETE FROM tasks WHERE updated_at < ? AND status = 'done'`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func firstLineOf(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
