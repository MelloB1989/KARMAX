package store

import (
	"database/sql"
	"time"
)

// StoredNotification is an agent update/alert surfaced in the app's notification
// feed. Every app.push is persisted as one of these so the feed survives a
// missed or undelivered push.
type StoredNotification struct {
	ID        string
	AgentID   string
	Kind      string
	Title     string
	Body      string
	Data      string // optional JSON blob delivered alongside the push
	CreatedAt time.Time
	ReadAt    *time.Time
}

const notificationCols = `id, agent_id, COALESCE(kind,''), COALESCE(title,''), ` +
	`body, COALESCE(data,''), created_at, read_at`

// CreateNotification persists a notification for the app feed.
func (s *Store) CreateNotification(n StoredNotification) error {
	_, err := s.db.Exec(
		`INSERT INTO notifications (id, agent_id, kind, title, body, data)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		n.ID, n.AgentID, n.Kind, n.Title, n.Body, n.Data,
	)
	return err
}

// ListNotifications returns notifications most-recent-first.
func (s *Store) ListNotifications(limit int) ([]StoredNotification, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+notificationCols+` FROM notifications ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanNotifications(rows)
}

// CountUnreadNotifications returns the number of unread notifications.
func (s *Store) CountUnreadNotifications() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM notifications WHERE read_at IS NULL`).Scan(&n)
	return n, err
}

// MarkNotificationRead marks a single notification read.
func (s *Store) MarkNotificationRead(id string) error {
	_, err := s.db.Exec(`UPDATE notifications SET read_at = datetime('now') WHERE id = ? AND read_at IS NULL`, id)
	return err
}

// MarkAllNotificationsRead marks every unread notification read.
func (s *Store) MarkAllNotificationsRead() error {
	_, err := s.db.Exec(`UPDATE notifications SET read_at = datetime('now') WHERE read_at IS NULL`)
	return err
}

func scanNotifications(rows *sql.Rows) ([]StoredNotification, error) {
	defer rows.Close()
	var out []StoredNotification
	for rows.Next() {
		var n StoredNotification
		var read sql.NullTime
		if err := rows.Scan(&n.ID, &n.AgentID, &n.Kind, &n.Title, &n.Body, &n.Data, &n.CreatedAt, &read); err != nil {
			return nil, err
		}
		if read.Valid {
			t := read.Time
			n.ReadAt = &t
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
