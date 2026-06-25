package store

import "time"

// StoredAppMessage is one turn in the phone-app conversation thread. This is
// kept separate from chat_history (the agent's full working memory, which also
// contains WhatsApp and loop turns) so the app shows only the user's own chat.
type StoredAppMessage struct {
	ID        string
	AgentID   string
	Role      string
	Content   string
	CreatedAt time.Time
}

// AppendAppMessage records one app conversation turn.
func (s *Store) AppendAppMessage(m StoredAppMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO app_messages (id, agent_id, role, content) VALUES (?, ?, ?, ?)`,
		m.ID, m.AgentID, m.Role, m.Content)
	return err
}

// LoadAppMessages returns the most recent `limit` app messages, oldest-first.
func (s *Store) LoadAppMessages(agentID string, limit int) ([]StoredAppMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`
		SELECT id, agent_id, role, content, created_at FROM (
			SELECT id, agent_id, role, content, created_at
			FROM app_messages WHERE agent_id = ?
			ORDER BY created_at DESC LIMIT ?
		) ORDER BY created_at ASC`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredAppMessage
	for rows.Next() {
		var m StoredAppMessage
		if err := rows.Scan(&m.ID, &m.AgentID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ClearAppMessages wipes the app conversation thread (start a new conversation).
func (s *Store) ClearAppMessages(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM app_messages WHERE agent_id = ?`, agentID)
	return err
}
