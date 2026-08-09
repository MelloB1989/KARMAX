package store

import (
	"time"
)

// Durable state for loop runs.
//
// A loop run used to be a goroutine and a log line: if it failed, the work was
// gone and the only trace was a WARN nobody reads. That is how three days of
// memory-merge and profile-refresh runs went missing while the daemon looked
// healthy — 141 failures, all the same dead model gateway, none of them
// retried and none of them visible.
//
// So a run is now a row. It is recorded before the work starts, updated when it
// ends, retried on a schedule if it failed, and dead-lettered if it keeps
// failing. `karmax status` reads these tables, which is what turns "the loop
// silently stopped" into something the operator can see.

// LoopRun is one execution of one loop.
type LoopRun struct {
	ID         string
	Loop       string
	Trigger    string
	Status     string // running | ok | failed | dead
	Attempt    int
	StartedAt  time.Time
	FinishedAt *time.Time
	DurationMS int64
	Error      string
}

// LoopState is the current standing of a loop, independent of any one run.
type LoopState struct {
	Loop string
	// LeaseUntil is how single-flight is enforced: a run holds the lease, and
	// a second attempt to start the same loop sees it and stands down. It has
	// an expiry rather than being a boolean so a crashed process cannot wedge
	// a loop closed forever.
	LeaseUntil    *time.Time
	LeaseOwner    string
	LastSuccessAt *time.Time
	LastFailureAt *time.Time
	ConsecFails   int
	NextRetryAt   *time.Time
	RetryAttempt  int
	LastError     string
}

// AcquireLoopLease takes the run lease for a loop, returning false when another
// run already holds it.
//
// The whole operation is one conditional UPDATE/INSERT so two runners racing
// cannot both win: checking and then writing would let both read "free".
func (s *Store) AcquireLoopLease(loop, owner string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	until := now.Add(ttl)

	// INSERT ... ON CONFLICT DO UPDATE ... WHERE is atomic in SQLite: the WHERE
	// on the upsert branch is what makes this a compare-and-set rather than a
	// blind overwrite.
	res, err := s.db.Exec(`
INSERT INTO loop_state (loop, lease_until, lease_owner)
VALUES (?, ?, ?)
ON CONFLICT(loop) DO UPDATE SET lease_until = excluded.lease_until, lease_owner = excluded.lease_owner
WHERE loop_state.lease_until IS NULL OR loop_state.lease_until < ?`,
		loop, until, owner, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ReleaseLoopLease frees the lease so the next scheduled tick can run.
func (s *Store) ReleaseLoopLease(loop, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Scoped to the owner: a run that overran its lease must not release the
	// lease a different run has since taken.
	_, err := s.db.Exec(
		`UPDATE loop_state SET lease_until = NULL, lease_owner = '' WHERE loop = ? AND lease_owner = ?`,
		loop, owner)
	return err
}

// StartLoopRun records that a run has begun.
func (s *Store) StartLoopRun(r LoopRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
INSERT INTO loop_runs (id, loop, trigger_kind, status, attempt, started_at)
VALUES (?, ?, ?, 'running', ?, ?)`,
		r.ID, r.Loop, r.Trigger, r.Attempt, r.StartedAt)
	return err
}

// FinishLoopRun records the outcome and updates the loop's standing.
//
// nextRetry nil means no retry is scheduled — either the run succeeded or it
// has exhausted its attempts and been dead-lettered.
func (s *Store) FinishLoopRun(id, loop, status, errMsg string, attempt int, nextRetry *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
UPDATE loop_runs
SET status = ?, finished_at = ?, error = ?,
    duration_ms = CAST((julianday(?) - julianday(started_at)) * 86400000 AS INTEGER)
WHERE id = ?`, status, now, errMsg, now, id); err != nil {
		return err
	}

	if status == "ok" {
		// Clearing the failure counters on success is what stops a loop that
		// recovered from being reported as broken forever.
		_, err = tx.Exec(`
INSERT INTO loop_state (loop, last_success_at, consec_fails, next_retry_at, retry_attempt, last_error)
VALUES (?, ?, 0, NULL, 0, '')
ON CONFLICT(loop) DO UPDATE SET
  last_success_at = excluded.last_success_at, consec_fails = 0,
  next_retry_at = NULL, retry_attempt = 0, last_error = ''`, loop, now)
	} else {
		_, err = tx.Exec(`
INSERT INTO loop_state (loop, last_failure_at, consec_fails, next_retry_at, retry_attempt, last_error)
VALUES (?, ?, 1, ?, ?, ?)
ON CONFLICT(loop) DO UPDATE SET
  last_failure_at = excluded.last_failure_at,
  consec_fails = loop_state.consec_fails + 1,
  next_retry_at = excluded.next_retry_at,
  retry_attempt = excluded.retry_attempt,
  last_error = excluded.last_error`, loop, now, nextRetry, attempt, errMsg)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// DueLoopRetries returns loops whose retry time has arrived.
func (s *Store) DueLoopRetries(now time.Time) ([]LoopState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
SELECT loop, retry_attempt, COALESCE(last_error, '')
FROM loop_state
WHERE next_retry_at IS NOT NULL AND next_retry_at <= ?`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LoopState
	for rows.Next() {
		var st LoopState
		if err := rows.Scan(&st.Loop, &st.RetryAttempt, &st.LastError); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ClearLoopRetry cancels a pending retry without recording an outcome. Used
// when a retry is being dispatched, so it is not dispatched twice.
func (s *Store) ClearLoopRetry(loop string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE loop_state SET next_retry_at = NULL WHERE loop = ?`, loop)
	return err
}

// LoopStates returns the standing of every loop that has ever run.
func (s *Store) LoopStates() ([]LoopState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
SELECT loop, lease_until, COALESCE(lease_owner,''), last_success_at, last_failure_at,
       consec_fails, next_retry_at, retry_attempt, COALESCE(last_error,'')
FROM loop_state ORDER BY loop`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LoopState
	for rows.Next() {
		var st LoopState
		if err := rows.Scan(&st.Loop, &st.LeaseUntil, &st.LeaseOwner, &st.LastSuccessAt,
			&st.LastFailureAt, &st.ConsecFails, &st.NextRetryAt, &st.RetryAttempt, &st.LastError); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// DeadLoopRuns returns runs that exhausted their retries — the work that was
// genuinely lost, which is the list an operator actually needs.
func (s *Store) DeadLoopRuns(limit int) ([]LoopRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
SELECT id, loop, trigger_kind, status, attempt, started_at, finished_at, COALESCE(error,'')
FROM loop_runs WHERE status = 'dead' ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LoopRun
	for rows.Next() {
		var r LoopRun
		if err := rows.Scan(&r.ID, &r.Loop, &r.Trigger, &r.Status, &r.Attempt,
			&r.StartedAt, &r.FinishedAt, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentLoopRuns returns the most recent runs for one loop, or all loops when
// loop is empty.
func (s *Store) RecentLoopRuns(loop string, limit int) ([]LoopRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT id, loop, trigger_kind, status, attempt, started_at, finished_at,
	             COALESCE(duration_ms,0), COALESCE(error,'')
	      FROM loop_runs`
	args := []any{}
	if loop != "" {
		q += ` WHERE loop = ?`
		args = append(args, loop)
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LoopRun
	for rows.Next() {
		var r LoopRun
		if err := rows.Scan(&r.ID, &r.Loop, &r.Trigger, &r.Status, &r.Attempt,
			&r.StartedAt, &r.FinishedAt, &r.DurationMS, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneLoopRuns drops run history older than the cutoff, keeping dead runs:
// those are the record of work that never happened, and deleting them by age
// would quietly erase the evidence.
func (s *Store) PruneLoopRuns(before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`DELETE FROM loop_runs WHERE started_at < ? AND status != 'dead'`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ReapStaleLoopRuns marks runs that were left "running" by a crash. Without it
// a killed daemon leaves rows that look like work still in progress forever.
func (s *Store) ReapStaleLoopRuns(olderThan time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`
UPDATE loop_runs SET status = 'dead', finished_at = ?,
  error = 'interrupted: the daemon stopped while this run was in progress'
WHERE status = 'running' AND started_at < ?`, time.Now(), olderThan)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
