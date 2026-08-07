package store

import (
	"database/sql"
	"time"
)

// The outbox that makes a remote memory layer safe to depend on.
//
// KARMAX writes memories from a daemon on someone's home internet. If a write
// went straight to the network, every blip would silently drop a fact. So a
// write is durable locally the moment it happens and the flusher's job is only
// to get it delivered — which turns "the network was down" from data loss into
// latency.

// OutboxItem is one pending delivery to GitLoom.
type OutboxItem struct {
	ID        string
	Namespace string
	Op        string // write | forget
	Payload   string // JSON gitloom.Memory for write; a repo path for forget
	Attempts  int
	LastError string
	CreatedAt time.Time
}

// EnqueueOutbox records a memory that still has to reach GitLoom.
func (s *Store) EnqueueOutbox(item OutboxItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item.Op == "" {
		item.Op = "write"
	}
	_, err := s.db.Exec(`
INSERT INTO gitloom_outbox (id, namespace, op, payload)
VALUES (?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET payload = excluded.payload, attempts = 0, last_error = ''`,
		item.ID, item.Namespace, item.Op, item.Payload)
	return err
}

// PendingOutbox returns the oldest undelivered items for a namespace.
//
// Oldest first so memories reach GitLoom in the order they were formed: a
// memory that supersedes another must not land before the one it replaces.
func (s *Store) PendingOutbox(namespace string, limit int) ([]OutboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`
SELECT id, namespace, op, payload, attempts, last_error, created_at
FROM gitloom_outbox WHERE namespace = ?
ORDER BY created_at ASC LIMIT ?`, namespace, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		if err := rows.Scan(&it.ID, &it.Namespace, &it.Op, &it.Payload,
			&it.Attempts, &it.LastError, &it.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// AckOutbox removes items GitLoom accepted.
func (s *Store) AckOutbox(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`DELETE FROM gitloom_outbox WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FailOutbox records a failed attempt so a permanently-bad item is visible
// rather than retried in silence forever.
func (s *Store) FailOutbox(ids []string, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE gitloom_outbox SET attempts = attempts + 1, last_error = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(reason, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CountOutbox reports how much is still undelivered — the number that says
// whether the memory layer is keeping up.
func (s *Store) CountOutbox(namespace string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM gitloom_outbox WHERE namespace = ?`, namespace).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

// SetGitloomPath records where an entry was filed, so a later update rewrites
// that file rather than creating a near-duplicate beside it.
func (s *Store) SetGitloomPath(id, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE memory_entries SET gitloom_path = ? WHERE id = ?`, path, id)
	return err
}

// GitloomPaths returns the GitLoom path for each of the given entry ids that
// has one. Used to resolve relationship links into paths.
func (s *Store) GitloomPaths(ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stmt, err := s.db.Prepare(`SELECT gitloom_path FROM memory_entries WHERE id = ? AND gitloom_path != ''`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, id := range ids {
		var p string
		if err := stmt.QueryRow(id).Scan(&p); err == nil && p != "" {
			out[id] = p
		}
	}
	return out, nil
}

// DeleteMemoryEntriesByGitloomPath removes the local rows that were filed at a
// GitLoom path. Plural because two local entries can reconcile onto one remote
// memory, and forgetting the memory must not leave either of them behind.
func (s *Store) DeleteMemoryEntriesByGitloomPath(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM memory_entries WHERE gitloom_path = ?`, path)
	return err
}
