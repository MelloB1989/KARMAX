package store

import (
	"database/sql"
	"time"
)

// StoredReview is one open "is this still relevant?" clarification question the
// agent raises about a stale memory / reminder / commitment.
type StoredReview struct {
	ID         string
	Namespace  string
	TargetKind string // memory | reminder | commitment | task
	TargetID   string
	DedupKey   string
	Question   string
	Options    string // JSON array
	Context    string
	Status     string // open | resolved | dismissed
	Answer     string
	Resolution string // kept | updated | forgotten | done | dropped
	CreatedAt  time.Time
	ResolvedAt *time.Time
}

// CreateReview inserts a new open review. The unique (namespace, dedup_key)
// index makes this a no-op-with-error for an item already asked about, so the
// same stale item is never surfaced twice — call HasReview first to check.
func (s *Store) CreateReview(r StoredReview) error {
	if r.Options == "" {
		r.Options = "[]"
	}
	if r.Status == "" {
		r.Status = "open"
	}
	_, err := s.db.Exec(
		`INSERT INTO reviews (id, namespace, target_kind, target_id, dedup_key, question, options, context, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Namespace, r.TargetKind, r.TargetID, r.DedupKey, r.Question, r.Options, r.Context, r.Status,
	)
	return err
}

// HasReview reports whether a review with this dedup key already exists in the
// namespace (in ANY status) — so a resolved/dismissed item is never re-asked.
func (s *Store) HasReview(namespace, dedupKey string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM reviews WHERE namespace = ? AND dedup_key = ?`, namespace, dedupKey).Scan(&n)
	return n > 0, err
}

// CountOpenReviews returns how many reviews are currently awaiting an answer —
// used to cap concurrent questions so "aggressive" never becomes spam.
func (s *Store) CountOpenReviews(namespace string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM reviews WHERE namespace = ? AND status = 'open'`, namespace).Scan(&n)
	return n, err
}

// ListOpenReviews returns the open reviews (oldest first) for a namespace.
func (s *Store) ListOpenReviews(namespace string, limit int) ([]StoredReview, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(
		`SELECT id, namespace, target_kind, target_id, dedup_key, question, options, context, status, answer, resolution, created_at, resolved_at
		 FROM reviews WHERE namespace = ? AND status = 'open' ORDER BY created_at ASC LIMIT ?`,
		namespace, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReviews(rows)
}

// GetReview loads one review by id.
func (s *Store) GetReview(id string) (*StoredReview, error) {
	rows, err := s.db.Query(
		`SELECT id, namespace, target_kind, target_id, dedup_key, question, options, context, status, answer, resolution, created_at, resolved_at
		 FROM reviews WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanReviews(rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// ResolveReview closes a review with the operator's answer and what was done
// (kept/updated/forgotten/done/dropped). Idempotent-ish: only affects an open
// review, so answering from a second channel after the first is a harmless
// no-op ("reply anywhere → dismiss everywhere").
func (s *Store) ResolveReview(id, status, answer, resolution string) error {
	if status == "" {
		status = "resolved"
	}
	_, err := s.db.Exec(
		`UPDATE reviews SET status = ?, answer = ?, resolution = ?, resolved_at = datetime('now')
		 WHERE id = ? AND status = 'open'`,
		status, answer, resolution, id,
	)
	return err
}

func scanReviews(rows *sql.Rows) ([]StoredReview, error) {
	var out []StoredReview
	for rows.Next() {
		var r StoredReview
		var resolvedAt sql.NullTime
		if err := rows.Scan(&r.ID, &r.Namespace, &r.TargetKind, &r.TargetID, &r.DedupKey,
			&r.Question, &r.Options, &r.Context, &r.Status, &r.Answer, &r.Resolution,
			&r.CreatedAt, &resolvedAt); err != nil {
			return nil, err
		}
		if resolvedAt.Valid {
			r.ResolvedAt = &resolvedAt.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
