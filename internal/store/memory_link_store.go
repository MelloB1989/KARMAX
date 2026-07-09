package store

import "time"

// MemoryLink is an LLM-generated relationship between two memory entries.
type MemoryLink struct {
	FromID   string `json:"from"`
	ToID     string `json:"to"`
	Relation string `json:"relation"`
}

// ReplaceMemoryLinks atomically replaces the whole relationship set.
func (s *Store) ReplaceMemoryLinks(links []MemoryLink) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM memory_links`); err != nil {
		tx.Rollback()
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO memory_links (from_id, to_id, relation) VALUES (?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, l := range links {
		if _, err := stmt.Exec(l.FromID, l.ToID, l.Relation); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ListMemoryLinks returns all stored relationships.
func (s *Store) ListMemoryLinks() ([]MemoryLink, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT from_id, to_id, relation FROM memory_links`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryLink
	for rows.Next() {
		var l MemoryLink
		if err := rows.Scan(&l.FromID, &l.ToID, &l.Relation); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// OldestMemoryLinkAge returns how long ago the current link set was built
// (based on the newest created_at — links are written as one atomic batch, so
// every row shares a timestamp). Returns -1 if there are no links. Used by the
// graph maintainer to detect a stale/frozen graph.
func (s *Store) OldestMemoryLinkAge() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var ts string
	err := s.db.QueryRow(`SELECT MAX(created_at) FROM memory_links`).Scan(&ts)
	if err != nil || ts == "" {
		return -1
	}
	// SQLite datetime('now') format, stored as UTC.
	t, perr := time.Parse("2006-01-02 15:04:05", ts)
	if perr != nil {
		return -1
	}
	return time.Since(t.UTC())
}
