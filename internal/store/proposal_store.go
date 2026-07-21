package store

import (
	"database/sql"
	"fmt"
	"time"
)

// StoredProposal is a human-in-the-loop action KARMAX wants approval for.
type StoredProposal struct {
	ID             string
	AgentID        string
	Kind           string
	Title          string
	Summary        string
	Context        string
	ProposedAction string
	Status         string // pending | approved | rejected | executed | failed
	DecisionNote   string
	Result         string
	CreatedAt      time.Time
	DecidedAt      *time.Time
}

const proposalCols = `id, agent_id, COALESCE(kind,''), title, COALESCE(summary,''), ` +
	`COALESCE(context,''), COALESCE(proposed_action,''), status, COALESCE(decision_note,''), ` +
	`COALESCE(result,''), created_at, decided_at`

// HasSimilarProposal reports whether a near-identical proposal already exists:
// still pending with the same title, OR any with the same title created within
// `within`. This stops the proxy/scan loops from flooding the inbox with
// duplicate "Decision — <same person>" approvals on every re-scan.
func (s *Store) HasSimilarProposal(title string, within time.Duration) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM proposals
		 WHERE title = ? AND (status = 'pending' OR created_at >= datetime('now', ?))`,
		title, fmt.Sprintf("-%d seconds", int(within.Seconds())),
	).Scan(&n)
	return n > 0, err
}

func (s *Store) CreateProposal(p StoredProposal) error {
	if p.Status == "" {
		p.Status = "pending"
	}
	_, err := s.db.Exec(
		`INSERT INTO proposals (id, agent_id, kind, title, summary, context, proposed_action, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.AgentID, p.Kind, p.Title, p.Summary, p.Context, p.ProposedAction, p.Status,
	)
	return err
}

func (s *Store) ListProposals(status string, limit int) ([]StoredProposal, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if status != "" {
		rows, err = s.db.Query(`SELECT `+proposalCols+` FROM proposals WHERE status = ? ORDER BY created_at DESC LIMIT ?`, status, limit)
	} else {
		rows, err = s.db.Query(`SELECT `+proposalCols+` FROM proposals ORDER BY created_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	return scanProposals(rows)
}

func (s *Store) GetProposal(id string) (*StoredProposal, error) {
	rows, err := s.db.Query(`SELECT `+proposalCols+` FROM proposals WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	ps, err := scanProposals(rows)
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	return &ps[0], nil
}

// DecideProposal records the user's approve/reject decision.
func (s *Store) DecideProposal(id, status, note string) error {
	_, err := s.db.Exec(
		`UPDATE proposals SET status = ?, decision_note = ?, decided_at = datetime('now') WHERE id = ?`,
		status, note, id,
	)
	return err
}

// SetProposalResult records the outcome after execution.
func (s *Store) SetProposalResult(id, status, result string) error {
	_, err := s.db.Exec(`UPDATE proposals SET status = ?, result = ? WHERE id = ?`, status, result, id)
	return err
}

func scanProposals(rows *sql.Rows) ([]StoredProposal, error) {
	defer rows.Close()
	var out []StoredProposal
	for rows.Next() {
		var p StoredProposal
		var decided sql.NullTime
		if err := rows.Scan(&p.ID, &p.AgentID, &p.Kind, &p.Title, &p.Summary, &p.Context,
			&p.ProposedAction, &p.Status, &p.DecisionNote, &p.Result, &p.CreatedAt, &decided); err != nil {
			return nil, err
		}
		if decided.Valid {
			t := decided.Time
			p.DecidedAt = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
