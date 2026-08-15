package store

import (
	"database/sql"
	"errors"
	"time"
)

// CommsCursor reports how far a channel has processed, and whether it has ever
// run. The bool matters: a first-ever start must NOT replay all of history, so
// "no cursor" and "cursor at the epoch" have to stay distinguishable.
func (s *Store) CommsCursor(channelID string) (time.Time, bool, error) {
	var at time.Time
	err := s.db.QueryRow(
		`SELECT last_seen_at FROM comms_cursors WHERE channel_id = ?`, channelID,
	).Scan(&at)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	return at, true, nil
}

// SetCommsCursor advances the channel's cursor. It never moves backwards: a
// replayed old message must not rewind past newer live traffic and cause the
// same window to be replayed again on the next restart.
func (s *Store) SetCommsCursor(channelID string, at time.Time, messageID string) error {
	_, err := s.db.Exec(`
		INSERT INTO comms_cursors (channel_id, last_seen_at, last_message_id, updated_at)
		VALUES (?, ?, ?, datetime('now'))
		ON CONFLICT(channel_id) DO UPDATE SET
			last_seen_at    = CASE WHEN excluded.last_seen_at > comms_cursors.last_seen_at
			                       THEN excluded.last_seen_at ELSE comms_cursors.last_seen_at END,
			last_message_id = CASE WHEN excluded.last_seen_at > comms_cursors.last_seen_at
			                       THEN excluded.last_message_id ELSE comms_cursors.last_message_id END,
			updated_at      = datetime('now')`,
		channelID, at.UTC(), messageID)
	return err
}
