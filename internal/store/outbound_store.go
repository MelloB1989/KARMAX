package store

import (
	"fmt"
	"time"
)

// ListOutboundSince is every outbound message stored at or after since,
// oldest first, across all channels. It is how a turn learns what it sent:
// every outbound passes through the store, whichever tool, shell call or
// delegated session produced it.
func (s *Store) ListOutboundSince(since time.Time, limit int) ([]StoredChannelMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`SELECT id, channel_id, channel_type, sender_id, sender_name, direction, content, reply_to_id, metadata, created_at
		FROM channel_messages WHERE direction = 'outbound' AND created_at >= ? ORDER BY created_at ASC LIMIT ?`,
		since, limit)
	if err != nil {
		return nil, fmt.Errorf("list outbound since: %w", err)
	}
	defer rows.Close()

	var out []StoredChannelMessage
	for rows.Next() {
		var m StoredChannelMessage
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.ChannelType, &m.SenderID, &m.SenderName, &m.Direction, &m.Content, &m.ReplyToID, &m.Metadata, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan channel message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
