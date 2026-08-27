package store

import (
	"database/sql"
	"strings"
	"time"
)

// What this organisation IS, as opposed to who is in it.
//
// org_members is the chart. This is the standing context an agent needs before
// it can answer anything usefully: what the company does, what its products are
// called, which tracker is real. Without it every agent starts every
// conversation knowing nothing about the company it works for, and asks
// questions a new hire would only ever ask once.

// DefaultOrg is the single organisation a self-hosted install serves.
const DefaultOrg = "default"

// OrgProfile is one organisation's standing description.
type OrgProfile struct {
	Org         string
	Name        string
	Domain      string
	Description string
	Timezone    string
	// Context is free text the agent is given on every turn — conventions,
	// vocabulary, who owns what. Deliberately unstructured: the useful things
	// to say about a company do not fit a schema, and a form with the wrong
	// fields collects worse answers than a blank page.
	Context   string
	UpdatedAt time.Time
	UpdatedBy string
}

// OrgProfileFor reads an organisation's profile, returning an empty one rather
// than nil when it has never been filled in — an unconfigured org is a normal
// state, not an error every caller has to branch on.
func (s *Store) OrgProfileFor(org string) (OrgProfile, error) {
	if org == "" {
		org = DefaultOrg
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var p OrgProfile
	var updatedAt sql.NullTime
	err := s.queryRow(`
SELECT org, name, domain, description, timezone, context, updated_at, updated_by
FROM org_profile WHERE org = ?`, org).
		Scan(&p.Org, &p.Name, &p.Domain, &p.Description, &p.Timezone, &p.Context, &updatedAt, &p.UpdatedBy)
	if err == sql.ErrNoRows {
		return OrgProfile{Org: org}, nil
	}
	if err != nil {
		return OrgProfile{}, err
	}
	if updatedAt.Valid {
		p.UpdatedAt = updatedAt.Time
	}
	return p, nil
}

// SaveOrgProfile writes an organisation's profile.
func (s *Store) SaveOrgProfile(p OrgProfile) error {
	if p.Org == "" {
		p.Org = DefaultOrg
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.exec(`
INSERT INTO org_profile (org, name, domain, description, timezone, context, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?, datetime('now'), ?)
ON CONFLICT(org) DO UPDATE SET
  name = excluded.name, domain = excluded.domain, description = excluded.description,
  timezone = excluded.timezone, context = excluded.context,
  updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		p.Org, strings.TrimSpace(p.Name), strings.TrimSpace(p.Domain),
		strings.TrimSpace(p.Description), strings.TrimSpace(p.Timezone),
		strings.TrimRight(p.Context, " \t\n"), p.UpdatedBy)
	return err
}

// Briefing renders the profile as the paragraph an agent is given each turn.
//
// Empty when nothing has been filled in: injecting a heading with nothing under
// it spends context to tell the model the company has no name.
func (p OrgProfile) Briefing() string {
	var b strings.Builder
	if p.Name != "" {
		b.WriteString("You work for " + p.Name)
		if p.Domain != "" {
			b.WriteString(" (" + p.Domain + ")")
		}
		b.WriteString(".")
	}
	if p.Description != "" {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(p.Description)
	}
	if p.Timezone != "" {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString("The team works in " + p.Timezone + ".")
	}
	if c := strings.TrimSpace(p.Context); c != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(c)
	}
	return b.String()
}
