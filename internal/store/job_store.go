package store

import (
	"time"
)

type StoredJob struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Cron      string     `json:"cron"`
	AgentID   string     `json:"agent_id"`
	Payload   string     `json:"payload"`
	Enabled   bool       `json:"enabled"`
	LastRun   *time.Time `json:"last_run,omitempty"`
	NextRun   *time.Time `json:"next_run,omitempty"`
	RunCount  int64      `json:"run_count"`
	CatchUp   bool       `json:"catch_up"`
	CreatedAt time.Time  `json:"created_at"`
}

func (s *Store) SaveJob(j StoredJob) error {
	payloadStr := j.Payload
	if payloadStr == "" {
		payloadStr = "{}"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	enabled := 0
	if j.Enabled {
		enabled = 1
	}
	catchUp := 0
	if j.CatchUp {
		catchUp = 1
	}

	_, err := s.db.Exec(`
		INSERT INTO scheduled_jobs (id, name, cron, agent_id, payload, enabled, last_run, next_run, run_count, catch_up)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, cron=excluded.cron, agent_id=excluded.agent_id,
			payload=excluded.payload, enabled=excluded.enabled, last_run=excluded.last_run,
			next_run=excluded.next_run, run_count=excluded.run_count, catch_up=excluded.catch_up`,
		j.ID, j.Name, j.Cron, j.AgentID, payloadStr, enabled, j.LastRun, j.NextRun, j.RunCount, catchUp,
	)
	return err
}

func (s *Store) ListJobs() ([]StoredJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id, name, cron, agent_id, payload, enabled, last_run, next_run, run_count, catch_up, created_at FROM scheduled_jobs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []StoredJob
	for rows.Next() {
		var j StoredJob
		var enabled, catchUp int
		if err := rows.Scan(&j.ID, &j.Name, &j.Cron, &j.AgentID, &j.Payload, &enabled, &j.LastRun, &j.NextRun, &j.RunCount, &catchUp, &j.CreatedAt); err != nil {
			return nil, err
		}
		j.Enabled = enabled == 1
		j.CatchUp = catchUp == 1
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func (s *Store) DeleteJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM scheduled_jobs WHERE id = ?`, id)
	return err
}

func (s *Store) UpdateJobRun(id string, lastRun time.Time, nextRun *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE scheduled_jobs SET last_run = ?, next_run = ?, run_count = run_count + 1 WHERE id = ?`, lastRun, nextRun, id)
	return err
}

func (s *Store) GetJob(id string) (*StoredJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRow(`SELECT id, name, cron, agent_id, payload, enabled, last_run, next_run, run_count, catch_up, created_at FROM scheduled_jobs WHERE id = ?`, id)
	var j StoredJob
	var enabled, catchUp int
	err := row.Scan(&j.ID, &j.Name, &j.Cron, &j.AgentID, &j.Payload, &enabled, &j.LastRun, &j.NextRun, &j.RunCount, &catchUp, &j.CreatedAt)
	if err != nil {
		return nil, err
	}
	j.Enabled = enabled == 1
	j.CatchUp = catchUp == 1
	return &j, nil
}
