package store

import (
	"database/sql"
	"time"
)

// Work the agent handed to a copy of itself.
//
// The parent link is why this exists. Background delegation already reports back
// through the event bus, but the only record of which turn caused which job was
// an in-memory map with a six-hour TTL — so a restart forgot the connection, and
// a job that outlived the process reported into a void.

// SubagentRun is one spawned child and its outcome.
type SubagentRun struct {
	ID         string
	ParentID   string
	ChildID    string
	Task       string
	Status     string // running | ok | failed | abandoned
	Result     string
	Depth      int
	StartedAt  time.Time
	FinishedAt *time.Time
}

// StartSubagentRun records a spawned child.
func (s *Store) StartSubagentRun(r SubagentRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}
	if r.Depth < 1 {
		r.Depth = 1
	}
	_, err := s.exec(`
INSERT INTO subagent_runs (id, parent_id, child_id, task, status, depth, started_at)
VALUES (?, ?, ?, ?, 'running', ?, ?)`,
		r.ID, r.ParentID, r.ChildID, r.Task, r.Depth, r.StartedAt)
	return err
}

// FinishSubagentRun closes a child with its outcome.
func (s *Store) FinishSubagentRun(id, status, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	_, err := s.exec(`
UPDATE subagent_runs SET status = ?, result = ?, finished_at = ? WHERE id = ?`,
		status, result, now, id)
	return err
}

// RunningSubagents counts a parent's live children, which is how the
// concurrency cap is enforced across restarts rather than only in memory.
func (s *Store) RunningSubagents(parentID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var n int
	err := s.queryRow(`SELECT COUNT(*) FROM subagent_runs WHERE parent_id = ? AND status = 'running'`, parentID).Scan(&n)
	return n, err
}

// SubagentRuns lists a parent's children, newest first.
func (s *Store) SubagentRuns(parentID string, limit int) ([]SubagentRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 20
	}
	rows, err := s.query(`
SELECT id, parent_id, child_id, task, status, result, depth, started_at, finished_at
FROM subagent_runs WHERE parent_id = ? ORDER BY started_at DESC LIMIT ?`, parentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SubagentRun
	for rows.Next() {
		var r SubagentRun
		var fin sql.NullTime
		if err := rows.Scan(&r.ID, &r.ParentID, &r.ChildID, &r.Task, &r.Status,
			&r.Result, &r.Depth, &r.StartedAt, &fin); err != nil {
			return nil, err
		}
		if fin.Valid {
			r.FinishedAt = &fin.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AbandonRunningSubagents closes children left running by a crash. They cannot
// still be running: their goroutines died with the process, and a row left open
// would count against the concurrency cap forever.
func (s *Store) AbandonRunningSubagents() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.exec(`
UPDATE subagent_runs SET status = 'abandoned', finished_at = ?, result = 'the daemon stopped while this was running'
WHERE status = 'running'`, time.Now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
