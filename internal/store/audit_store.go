package store

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Who asked for what, and what was done about it. An agent acting inside a
// company with real write access is answerable, and this table is the whole of
// the answer.

// AuditEvent is one decision an agent made or acted on.
type AuditEvent struct {
	ID, ActorKind, ActorID, Agent, CaseID        string
	Recipe, Step, Verb, Target, Decision, Detail string
	CreatedAt                                    time.Time
}

// AuditFilter narrows a QueryAudit call. Zero-value fields are not applied.
type AuditFilter struct {
	ActorID, Agent, CaseID, Verb string

	// VerbLike matches a substring of the verb instead of the whole thing.
	// The console's audit filter is a free-text box — someone types "merge"
	// expecting to see pr.merge — and exact match would return nothing for
	// every partial word. Applied in addition to Verb, not instead of it.
	VerbLike string

	Since time.Time
	Limit int
}

// AppendAudit records one decision.
func (s *Store) AppendAudit(e AuditEvent) error {
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO audit_events (id, actor_kind, actor_id, agent, case_id, recipe, step, verb, target, decision, detail, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ActorKind, e.ActorID, e.Agent, e.CaseID, e.Recipe, e.Step, e.Verb, e.Target, e.Decision, e.Detail, e.CreatedAt)
	return err
}

// QueryAudit applies only the filter's non-zero fields, newest first.
func (s *Store) QueryAudit(f AuditFilter) ([]AuditEvent, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := `SELECT id, actor_kind, actor_id, agent, case_id, recipe, step, verb, target, decision, detail, created_at FROM audit_events`
	var conds []string
	var args []any
	if f.ActorID != "" {
		conds = append(conds, "actor_id = ?")
		args = append(args, f.ActorID)
	}
	if f.Agent != "" {
		conds = append(conds, "agent = ?")
		args = append(args, f.Agent)
	}
	if f.CaseID != "" {
		conds = append(conds, "case_id = ?")
		args = append(args, f.CaseID)
	}
	if f.Verb != "" {
		conds = append(conds, "verb = ?")
		args = append(args, f.Verb)
	}
	if f.VerbLike != "" {
		conds = append(conds, `verb LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(f.VerbLike)+"%")
	}
	if !f.Since.IsZero() {
		conds = append(conds, "created_at >= ?")
		args = append(args, f.Since)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, f.Limit)

	rows, err := s.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.ID, &e.ActorKind, &e.ActorID, &e.Agent, &e.CaseID,
			&e.Recipe, &e.Step, &e.Verb, &e.Target, &e.Decision, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// escapeLike neutralises the wildcards in a user-supplied LIKE pattern, so a
// filter of "%" means the literal character rather than "everything".
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(s)
}
