package store

import (
	"fmt"
	"strings"
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

	_, err := s.exec(`
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

	rows, err := s.query(`SELECT id, type, agent_id, config_json, status, created_at, updated_at FROM communication_channels ORDER BY created_at DESC`)
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

	_, err := s.exec(`UPDATE communication_channels SET status = ?, updated_at = datetime('now') WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("update channel status: %w", err)
	}
	return nil
}

func (s *Store) SaveChannelMessage(msg StoredChannelMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.exec(`INSERT INTO channel_messages (id, channel_id, channel_type, sender_id, sender_name, direction, content, reply_to_id, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.ChannelID, msg.ChannelType, msg.SenderID, msg.SenderName, msg.Direction, msg.Content, msg.ReplyToID, msg.Metadata)
	if err != nil {
		return fmt.Errorf("save channel message: %w", err)
	}
	return nil
}

func (s *Store) ListChannelMessages(channelID string, limit int) ([]StoredChannelMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`SELECT id, channel_id, channel_type, sender_id, sender_name, direction, content, reply_to_id, metadata, created_at FROM channel_messages WHERE channel_id = ? ORDER BY created_at DESC LIMIT ?`,
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

// HeardFromSince reports whether a chat has sent anything after a moment.
func (s *Store) HeardFromSince(chatID string, since time.Time) bool {
	rows, err := s.ListChannelMessages(chatID, 10)
	if err != nil {
		return false
	}
	for _, m := range rows {
		if strings.EqualFold(m.Direction, "inbound") && m.CreatedAt.After(since) {
			return true
		}
	}
	return false
}

// sentWindow is how far a message wacli shows can be from the moment KARMAX
// recorded sending it and still be the same message.
const sentWindow = 5 * time.Minute

// SentWhatsApp reports whether KARMAX itself sent text to a WhatsApp chat
// around at. KARMAX speaks through the operator's own account, so wacli shows
// its messages as the operator's; this is how they are told apart.
func (s *Store) SentWhatsApp(chatJID, text string, at time.Time) bool {
	user, _, _ := strings.Cut(chatJID, "@")
	user, _, _ = strings.Cut(user, ":")
	text = strings.TrimSpace(text)
	if user == "" || text == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	err := s.queryRow(`SELECT COUNT(*) FROM channel_messages
WHERE direction = 'outbound' AND channel_type = 'whatsapp'
  AND (channel_id = ? OR channel_id LIKE ?)
  AND trim(content) = ? AND created_at BETWEEN ? AND ?`,
		user, user+"@%", text,
		at.Add(-sentWindow).UTC().Format("2006-01-02 15:04:05"),
		at.Add(sentWindow).UTC().Format("2006-01-02 15:04:05")).Scan(&n)
	return err == nil && n > 0
}

// SentAsOperator reports whether KARMAX itself sent text on a service as one
// of the operator's logins around at — as the sent-as recorder keeps every
// such send, with the login it was made as. KARMAX speaks there through the
// operator's own account, so the service shows its messages as the
// operator's; this is how they are told apart.
func (s *Store) SentAsOperator(service, account, text string, at time.Time) bool {
	text = strings.Join(strings.Fields(text), " ")
	account = strings.ToLower(strings.TrimSpace(account))
	if service == "" || account == "" || text == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	err := s.queryRow(`SELECT COUNT(*) FROM channel_messages
WHERE direction = 'outbound' AND channel_type = ? AND lower(sender_name) = ?
  AND trim(content) = ? AND created_at BETWEEN ? AND ?`,
		service, account, text,
		at.Add(-sentWindow).UTC().Format("2006-01-02 15:04:05"),
		at.Add(sentWindow).UTC().Format("2006-01-02 15:04:05")).Scan(&n)
	return err == nil && n > 0
}
