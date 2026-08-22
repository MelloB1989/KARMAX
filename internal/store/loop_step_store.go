package store

import (
	"database/sql"
	"time"
)

// Step checkpoints, keyed by execution rather than by run — the run id changes
// per attempt, the execution does not, which is what lets a retry skip work
// that already happened.

// SaveLoopStep records a completed step and what it produced.
func (s *Store) SaveLoopStep(executionID, loop, name, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO loop_steps (execution_id, loop, name, result, completed_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(execution_id, name) DO UPDATE SET
  result = excluded.result, completed_at = excluded.completed_at`,
		executionID, loop, name, result, time.Now())
	return err
}

// LoopStep returns a step's result, and whether it ran at all.
func (s *Store) LoopStep(executionID, name string) (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result string
	err := s.queryRow(
		`SELECT result FROM loop_steps WHERE execution_id = ? AND name = ?`,
		executionID, name).Scan(&result)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return result, true, nil
}

// CompletedSteps counts what an execution has got through.
func (s *Store) CompletedSteps(executionID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var n int
	err := s.queryRow(
		`SELECT COUNT(*) FROM loop_steps WHERE execution_id = ?`, executionID).Scan(&n)
	return n, err
}

// ClearLoopSteps drops an execution's checkpoints once it has finished for good.
func (s *Store) ClearLoopSteps(executionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`DELETE FROM loop_steps WHERE execution_id = ?`, executionID)
	return err
}

// SetLoopExecution records which execution a loop is working on; "" clears it.
func (s *Store) SetLoopExecution(loop, executionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO loop_state (loop, execution_id) VALUES (?, ?)
ON CONFLICT(loop) DO UPDATE SET execution_id = excluded.execution_id`,
		loop, executionID)
	return err
}

// LoopExecution returns the execution a loop is part-way through, or "".
func (s *Store) LoopExecution(loop string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var id sql.NullString
	err := s.queryRow(`SELECT execution_id FROM loop_state WHERE loop = ?`, loop).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id.String, err
}

// PruneLoopSteps drops checkpoints left by executions that never finished
// cleanly — a crash between the last step and the clear.
func (s *Store) PruneLoopSteps(before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.exec(`DELETE FROM loop_steps WHERE completed_at < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
