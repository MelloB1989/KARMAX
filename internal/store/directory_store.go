package store

import "database/sql"

// Slack user -> org member. An agent acting for an employee has to be able to
// name that employee, or the audit trail says only "the agent did it".

// Member maps one external identity (a Slack user, a Jira reporter, a GitHub
// login) onto an org member.
type Member struct{ ExternalKind, ExternalID, Member, Org, Name string }

// MapMember records or updates an external identity's mapping.
func (s *Store) MapMember(m Member) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO directory (external_kind, external_id, member, org, name, updated_at)
VALUES (?, ?, ?, ?, ?, datetime('now'))
ON CONFLICT(external_kind, external_id) DO UPDATE SET
  member = excluded.member, org = excluded.org, name = excluded.name, updated_at = excluded.updated_at`,
		m.ExternalKind, m.ExternalID, m.Member, m.Org, m.Name)
	return err
}

// MemberByExternal looks up the org member behind one external identity.
func (s *Store) MemberByExternal(kind, id string) (Member, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var m Member
	err := s.queryRow(`
SELECT external_kind, external_id, member, org, name FROM directory WHERE external_kind = ? AND external_id = ?`,
		kind, id).Scan(&m.ExternalKind, &m.ExternalID, &m.Member, &m.Org, &m.Name)
	if err == sql.ErrNoRows {
		return Member{}, false, nil
	}
	if err != nil {
		return Member{}, false, err
	}
	return m, true, nil
}

// ListDirectory returns every mapping of one external kind, or every mapping
// when kind is empty.
func (s *Store) ListDirectory(kind string) ([]Member, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := `SELECT external_kind, external_id, member, org, name FROM directory`
	var args []any
	if kind != "" {
		q += ` WHERE external_kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY external_kind, external_id`

	rows, err := s.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ExternalKind, &m.ExternalID, &m.Member, &m.Org, &m.Name); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
