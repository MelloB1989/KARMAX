package store

import (
	"fmt"
	"time"
)

type StoredChannel struct {
	ID         string
	Type       string
	AgentID    string
	ConfigJSON string
	Status     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type StoredChannelMessage struct {
	ID          string
	ChannelID   string
	ChannelType string
	SenderID    string
	SenderName  string
	Direction   string
	Content     string
	ReplyToID   string
	Metadata    string
	CreatedAt   time.Time
}

func (s *Store) SaveChannel(ch StoredChannel) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO communication_channels (id, type, agent_id, config_json, status, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			type=excluded.type,
			agent_id=excluded.agent_id,
			config_json=excluded.config_json,
			status=excluded.status,
			updated_at=datetime('now')`,
		ch.ID, ch.Type, ch.AgentID, ch.ConfigJSON, ch.Status)
	if err != nil {
		return fmt.Errorf("save channel: %w", err)
	}
	return nil
}

func (s *Store) ListChannels() ([]StoredChannel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id, type, agent_id, config_json, status, created_at, updated_at FROM communication_channels ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()

	var channels []StoredChannel
	for rows.Next() {
		var ch StoredChannel
		if err := rows.Scan(&ch.ID, &ch.Type, &ch.AgentID, &ch.ConfigJSON, &ch.Status, &ch.CreatedAt, &ch.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		channels = append(channels, ch)
	}
	return channels, nil
}

func (s *Store) UpdateChannelStatus(id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`UPDATE communication_channels SET status = ?, updated_at = datetime('now') WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("update channel status: %w", err)
	}
	return nil
}

func (s *Store) SaveChannelMessage(msg StoredChannelMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`INSERT INTO channel_messages (id, channel_id, channel_type, sender_id, sender_name, direction, content, reply_to_id, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.ChannelID, msg.ChannelType, msg.SenderID, msg.SenderName, msg.Direction, msg.Content, msg.ReplyToID, msg.Metadata)
	if err != nil {
		return fmt.Errorf("save channel message: %w", err)
	}
	return nil
}

func (s *Store) ListChannelMessages(channelID string, limit int) ([]StoredChannelMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id, channel_id, channel_type, sender_id, sender_name, direction, content, reply_to_id, metadata, created_at FROM channel_messages WHERE channel_id = ? ORDER BY created_at DESC LIMIT ?`,
		channelID, limit)
	if err != nil {
		return nil, fmt.Errorf("list channel messages: %w", err)
	}
	defer rows.Close()

	var messages []StoredChannelMessage
	for rows.Next() {
		var m StoredChannelMessage
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.ChannelType, &m.SenderID, &m.SenderName, &m.Direction, &m.Content, &m.ReplyToID, &m.Metadata, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan channel message: %w", err)
		}
		messages = append(messages, m)
	}
	return messages, nil
}
