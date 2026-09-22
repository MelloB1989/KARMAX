package store

// The ledger that stops anybody being messaged twice.
//
// This exists because the alternative was tried and failed. The discipline —
// write down that you are about to contact somebody, contact them, write down
// that you did — lived in prose, an agent wrote its own version of it, and the
// version it wrote dropped the parts that mattered. Nothing detected that
// until the platform did.
//
// So it is a table with a primary key on campaign+target. Claiming a target is
// an INSERT: if the row is already there the insert fails, and that failure IS
// the answer. There is no code path that sends without claiming first, and no
// way for a caller to decide not to bother.

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Outreach states. A target is claimed before the send and settled after it,
// so an attempt interrupted halfway leaves `attempted` behind — which still
// blocks a retry, because "we do not know whether that message arrived" is a
// reason not to send it again rather than a reason to.
const (
	OutreachAttempted = "attempted"
	OutreachSent      = "sent"
	OutreachFailed    = "failed"
	// OutreachStopped marks a campaign the platform told us to stop. It is
	// recorded against a target so the reason survives, and checked for the
	// whole campaign before any further send.
	OutreachStopped = "stopped"
)

type OutreachRecord struct {
	Campaign  string
	Channel   string
	Target    string
	State     string
	Detail    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func outreachID(campaign, target string) string {
	return strings.TrimSpace(campaign) + "\x00" + strings.TrimSpace(target)
}

// ClaimOutreach reserves one target, returning false when it was already
// claimed by this campaign.
//
// The INSERT is the lock. Two callers racing at the same recipient — two
// agents, or one agent retried — produce one winner and one false, decided by
// the database rather than by whichever read the ledger last.
func (s *Store) ClaimOutreach(campaign, channel, target string) (bool, error) {
	if strings.TrimSpace(campaign) == "" || strings.TrimSpace(target) == "" {
		return false, fmt.Errorf("a claim needs a campaign and a target")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO outreach_ledger (id, campaign, channel, target, state, detail, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, '', datetime('now'), datetime('now'))`,
		outreachID(campaign, target), campaign, channel, target, OutreachAttempted)
	if err != nil {
		// A duplicate key is the ordinary answer here, not a fault: it means
		// this person has already been dealt with.
		if isDuplicate(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ReleaseOutreach drops a claim that never became a send — the sign-in failed,
// or the run was cancelled while pacing. Only a claim still `attempted` goes:
// once a send has settled, its row is the record that it happened.
func (s *Store) ReleaseOutreach(campaign, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`DELETE FROM outreach_ledger WHERE id = ? AND state = ?`,
		outreachID(campaign, target), OutreachAttempted)
	return err
}

// SettleOutreach records how a claimed target turned out.
func (s *Store) SettleOutreach(campaign, target, state, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(
		`UPDATE outreach_ledger SET state = ?, detail = ?, updated_at = datetime('now') WHERE id = ?`,
		state, trimDetail(detail), outreachID(campaign, target))
	return err
}

// CountOutreach returns how many targets a campaign has claimed, which is what
// a per-run cap is measured against. Claims rather than successes on purpose:
// a cap that only counted successes would let a campaign that is failing
// hammer away without limit.
func (s *Store) CountOutreach(campaign string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	err := s.queryRow(`SELECT COUNT(*) FROM outreach_ledger WHERE campaign = ?`, campaign).Scan(&n)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	return n, nil
}

// CampaignStopped reports whether this campaign already hit a hard stop, and
// why.
//
// Checked before every send rather than remembered in the process that saw it:
// a hard stop means the platform is refusing this account, and that fact has
// to outlive the run, the restart, and whichever other run tries next.
func (s *Store) CampaignStopped(campaign string) (bool, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var detail sql.NullString
	err := s.queryRow(
		`SELECT detail FROM outreach_ledger WHERE campaign = ? AND state = ? LIMIT 1`,
		campaign, OutreachStopped).Scan(&detail)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, detail.String, nil
}

// ListOutreach returns a campaign's ledger, newest first.
func (s *Store) ListOutreach(campaign string, limit int) ([]OutreachRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.query(`
SELECT campaign, channel, target, state, detail, created_at, updated_at
FROM outreach_ledger WHERE campaign = ? ORDER BY created_at DESC LIMIT ?`, campaign, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []OutreachRecord{}
	for rows.Next() {
		var r OutreachRecord
		var detail sql.NullString
		if err := rows.Scan(&r.Campaign, &r.Channel, &r.Target, &r.State, &detail,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Detail = detail.String
		out = append(out, r)
	}
	return out, rows.Err()
}

func trimDetail(s string) string {
	const max = 500
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// isDuplicate recognises a primary-key collision across the backends in use.
// Dialect.Redundant covers the migration cases; this covers the insert ones,
// whose wording differs between SQLite and MySQL.
func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique constraint") ||
		strings.Contains(m, "duplicate entry") ||
		strings.Contains(m, "duplicate key")
}
