package store

import (
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// A container is state the daemon does not hold. Recording the id is what lets
// a restart reconcile what is still running instead of leaking it.

// SandboxRun is one launch of a sandbox driver.
type SandboxRun struct {
	ID, CaseID, Driver, ContainerID, Image, Status string
	Repo, Branch, Task, Error, LogTail             string
	ExitCode                                       int
	StartedAt                                      time.Time
	FinishedAt                                     *time.Time
}

const sandboxRunColumns = `id, case_id, driver, container_id, image, status, repo, branch, task, exit_code, error, log_tail, started_at, finished_at`

func scanSandboxRun(row rowScanner) (SandboxRun, error) {
	var r SandboxRun
	var finishedAt sql.NullTime
	if err := row.Scan(&r.ID, &r.CaseID, &r.Driver, &r.ContainerID, &r.Image, &r.Status,
		&r.Repo, &r.Branch, &r.Task, &r.ExitCode, &r.Error, &r.LogTail, &r.StartedAt, &finishedAt); err != nil {
		return SandboxRun{}, err
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		r.FinishedAt = &t
	}
	return r, nil
}

// sandboxTerminal reports whether a status ends a run — the point at which
// finished_at is set.
func sandboxTerminal(status string) bool {
	switch status {
	case "exited", "failed", "gone":
		return true
	}
	return false
}

// StartSandboxRun records that a container launch has begun.
func (s *Store) StartSandboxRun(r SandboxRun) error {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	if r.Status == "" {
		r.Status = "starting"
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO sandbox_runs (id, case_id, driver, container_id, image, status, repo, branch, task, exit_code, error, log_tail, started_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', '', ?, NULL)`,
		r.ID, r.CaseID, r.Driver, r.ContainerID, r.Image, r.Status, r.Repo, r.Branch, r.Task, r.StartedAt)
	return err
}

// UpdateSandboxRun records a poll's result. finished_at is set only on a
// terminal status (exited/failed/gone); a driver still starting or running
// leaves it null so LiveSandboxRuns keeps finding the row.
func (s *Store) UpdateSandboxRun(id, status string, exitCode int, errMsg, logTail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sandboxTerminal(status) {
		_, err := s.exec(`
UPDATE sandbox_runs SET status = ?, exit_code = ?, error = ?, log_tail = ?, finished_at = ?
WHERE id = ?`, status, exitCode, errMsg, logTail, time.Now(), id)
		return err
	}
	_, err := s.exec(`
UPDATE sandbox_runs SET status = ?, exit_code = ?, error = ?, log_tail = ?
WHERE id = ?`, status, exitCode, errMsg, logTail, id)
	return err
}

// LiveSandboxRuns returns runs still starting or running — the set a restart
// reconciles against the containers it actually finds.
func (s *Store) LiveSandboxRuns() ([]SandboxRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT ` + sandboxRunColumns + `
FROM sandbox_runs WHERE status IN ('starting', 'running') ORDER BY started_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SandboxRun
	for rows.Next() {
		r, err := scanSandboxRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SandboxRun looks a run up by id.
func (s *Store) SandboxRun(id string) (SandboxRun, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	r, err := scanSandboxRun(s.queryRow(`SELECT `+sandboxRunColumns+` FROM sandbox_runs WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return SandboxRun{}, false, nil
	}
	if err != nil {
		return SandboxRun{}, false, err
	}
	return r, true, nil
}

// ListSandboxRuns returns a case's runs, most recent first.
func (s *Store) ListSandboxRuns(caseID string, limit int) ([]SandboxRun, error) {
	if limit <= 0 {
		limit = 50
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT `+sandboxRunColumns+`
FROM sandbox_runs WHERE case_id = ? ORDER BY started_at DESC LIMIT ?`, caseID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SandboxRun
	for rows.Next() {
		r, err := scanSandboxRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
