package store

import (
	"database/sql"
	"strings"
	"time"
)

// The org chart, as data.
//
// Hiring an agent into a department should be a config action, not a code
// change — so departments and members are rows, and everything that depends on
// the shape of the org reads them rather than hardcoding it.

// OrgMember is one instance's place in an organisation.
type OrgMember struct {
	Org        string // the org's signing key
	Member     string // the member instance's signing key
	Name       string
	Department string
	Role       string
	// Namespace is the GitLoom namespace this member reads and writes, which is
	// how org → department → individual memory is partitioned.
	Namespace string
	AddedAt   time.Time
}

// SaveOrgMember records or updates someone's place in the org.
func (s *Store) SaveOrgMember(m OrgMember) error {
	if m.AddedAt.IsZero() {
		m.AddedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO org_members (org, member, name, department, role, namespace, added_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(org, member) DO UPDATE SET
  name = excluded.name, department = excluded.department,
  role = excluded.role, namespace = excluded.namespace`,
		m.Org, m.Member, m.Name, m.Department, m.Role, m.Namespace, m.AddedAt)
	return err
}

// RemoveOrgMember takes someone out of the org chart.
func (s *Store) RemoveOrgMember(org, member string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`DELETE FROM org_members WHERE org = ? AND member = ?`, org, member)
	return err
}

// OrgMembers lists an org's members, by department then name.
func (s *Store) OrgMembers(org string) ([]OrgMember, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT org, member, name, department, role, namespace, added_at
FROM org_members WHERE org = ? ORDER BY department, name`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMembers(rows)
}

// OrgMemberByKey returns one member, whichever org they belong to.
func (s *Store) OrgMemberByKey(member string) (*OrgMember, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var (
		m              OrgMember
		dept, role, ns sql.NullString
	)
	err := s.queryRow(`
SELECT org, member, name, department, role, namespace, added_at
FROM org_members WHERE member = ? LIMIT 1`, member).
		Scan(&m.Org, &m.Member, &m.Name, &dept, &role, &ns, &m.AddedAt)
	if err != nil {
		return nil, err
	}
	m.Department, m.Role, m.Namespace = dept.String, role.String, ns.String
	return &m, nil
}

// Departments lists the distinct departments in an org.
func (s *Store) Departments(org string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT DISTINCT department FROM org_members
WHERE org = ? AND department != '' ORDER BY department`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanMembers(rows *sql.Rows) ([]OrgMember, error) {
	var out []OrgMember
	for rows.Next() {
		var m OrgMember
		var dept, role, ns sql.NullString
		if err := rows.Scan(&m.Org, &m.Member, &m.Name, &dept, &role, &ns, &m.AddedAt); err != nil {
			return nil, err
		}
		m.Department, m.Role, m.Namespace = dept.String, role.String, ns.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// OrgNamespace is the memory namespace for a department, or the org itself when
// department is empty. Kept here so every caller derives it the same way — two
// spellings of a namespace means two memories nobody can find.
func OrgNamespace(orgName, department string) string {
	orgName = slug(orgName)
	if department == "" {
		return "org-" + orgName
	}
	return "org-" + orgName + "-" + slug(department)
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '-', r == '_':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
