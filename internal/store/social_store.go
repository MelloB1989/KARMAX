package store

import (
	"database/sql"
	"time"
)

// SocialPost is one attempt to say something in the operator's name.
type SocialPost struct {
	Platform string    `json:"platform"`
	PostedAt time.Time `json:"posted_at"`
	Status   string    `json:"status"` // posted | refused | failed
	PostID   string    `json:"post_id,omitempty"`
	Text     string    `json:"text"`
	Detail   string    `json:"detail,omitempty"`
}

// RecordPost writes what was said, or what was stopped.
//
// Refusals are recorded as deliberately as successes. A guard that quietly
// blocks things teaches nobody anything; the operator should be able to see
// that KARMAX tried to post something naming a client and did not.
func (s *Store) RecordPost(p SocialPost) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO social_posts (platform, status, post_id, text, detail) VALUES (?,?,?,?,?)`,
		p.Platform, p.Status, p.PostID, p.Text, p.Detail)
	return err
}

// CountPostsSince returns how many posts actually went out on a platform.
//
// Only successes count against the rate limit: a refused draft did not use
// anybody's attention, and counting it would let a run of bad drafts silence
// the day's real post.
func (s *Store) CountPostsSince(platform string, since time.Time) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM social_posts WHERE platform = ? AND status = 'posted' AND posted_at >= ?`,
		platform, since.UTC()).Scan(&n)
	return n, err
}

// LastPostAt is when the platform last had something published to it.
func (s *Store) LastPostAt(platform string) (time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var t sql.NullTime
	err := s.db.QueryRow(
		`SELECT MAX(posted_at) FROM social_posts WHERE platform = ? AND status = 'posted'`,
		platform).Scan(&t)
	if err != nil || !t.Valid {
		return time.Time{}, err
	}
	return t.Time, nil
}

// RecentPosts returns the latest attempts, newest first, for the operator.
func (s *Store) RecentPosts(limit int) ([]SocialPost, error) {
	if limit <= 0 {
		limit = 20
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(
		`SELECT platform, posted_at, status, post_id, text, detail
		 FROM social_posts ORDER BY posted_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SocialPost
	for rows.Next() {
		var p SocialPost
		if err := rows.Scan(&p.Platform, &p.PostedAt, &p.Status, &p.PostID, &p.Text, &p.Detail); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
