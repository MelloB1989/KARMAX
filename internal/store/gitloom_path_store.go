package store

// Where a local memory ended up in GitLoom.
//
// What remains of the mirror. Memories no longer live in SQLite at all when
// GitLoom is configured — but memory_entries still holds everything written
// before the move, and `karmax migrate gitloom` is what carries those across.
// It records each entry's destination as it goes, so an interrupted run
// resumes rather than re-sending what it already delivered.

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
