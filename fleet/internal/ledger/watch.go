package ledger

import (
	"database/sql"
	"time"
)

// The ledger's reads for the watch snapshot (daemon.Fleet.Snapshot): each
// is one query, made at most once a second however many watch.

// TaskLine is a task with its thread's size and newest message, for a list.
type TaskLine struct {
	Task
	Messages int
	Last     string // the newest message's text, whole
	LastAt   time.Time
	LastKind string
}

// TaskLines is the tasks still open and those closed since a time, newest
// assignment first, each with its thread's size and newest message.
func (db *DB) TaskLines(closedSince time.Time) ([]TaskLine, error) {
	rows, err := db.sql.Query(`WITH t AS (
		SELECT id, COALESCE(agent,'') agent, COALESCE(assigner,'') assigner, assigned, closed, COALESCE(outcome,'') outcome,
			COALESCE(branch,'') branch, COALESCE(pr,'') pr, COALESCE(repo,'') repo, COALESCE(model,'') model
		FROM tasks WHERE agent IS NOT NULL AND (closed IS NULL OR closed >= ?)),
	m AS (SELECT task, COUNT(*) n, MAX(id) last FROM relays WHERE task IN (SELECT id FROM t) GROUP BY task)
	SELECT t.id, t.agent, t.assigner, t.assigned, t.closed, t.outcome, t.branch, t.pr, t.repo, t.model,
		COALESCE(m.n,0), COALESCE(r.text,''), r.at, COALESCE(r.kind,'')
	FROM t LEFT JOIN m ON m.task = t.id LEFT JOIN relays r ON r.id = m.last
	ORDER BY t.assigned DESC, t.id DESC`, closedSince.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskLine{}
	for rows.Next() {
		var l TaskLine
		var as, cl, at sql.NullInt64
		if err := rows.Scan(&l.ID, &l.Agent, &l.Assigner, &as, &cl, &l.Outcome, &l.Branch, &l.PR, &l.Repo, &l.Model,
			&l.Messages, &l.Last, &at, &l.LastKind); err != nil {
			return nil, err
		}
		l.Assigned, l.Closed, l.LastAt = fromUnix(as), fromUnix(cl), fromUnix(at)
		if l.Messages > 0 && l.LastKind == "" {
			l.LastKind = KindReport
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// QuotaSince is every quota sample since a time, by account, oldest first.
func (db *DB) QuotaSince(since time.Time) ([]Quota, error) {
	rows, err := db.sql.Query(`SELECT account, at, COALESCE(five_hour,0), five_resets, COALESCE(seven_day,0), seven_resets, COALESCE(source,'')
		FROM quota WHERE at >= ? ORDER BY account, at`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Quota
	for rows.Next() {
		var q Quota
		var at, fr, sr sql.NullInt64
		if err := rows.Scan(&q.Account, &at, &q.FiveHour, &fr, &q.SevenDay, &sr, &q.Source); err != nil {
			return nil, err
		}
		q.At, q.FiveResets, q.SevenResets = fromUnix(at), fromUnix(fr), fromUnix(sr)
		out = append(out, q)
	}
	return out, rows.Err()
}

// Restarts counts each agent's container starts (fleet.container_start)
// since a time.
func (db *DB) Restarts(since time.Time) (map[string]int, error) {
	rows, err := db.sql.Query(`SELECT agent, COUNT(*) FROM events WHERE kind = 'fleet.container_start' AND at >= ? GROUP BY agent`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var a string
		var n int
		if err := rows.Scan(&a, &n); err != nil {
			return nil, err
		}
		out[a] = n
	}
	return out, rows.Err()
}
