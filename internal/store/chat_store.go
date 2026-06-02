package store

import (
	"fmt"
	"time"
)

type StoredChatMessage struct {
	ID         string
	AgentID    string
	Role       string
	Content    string
	ToolCalls  string
	ToolCallID string
	Tokens     int
	Metadata   string
	CreatedAt  time.Time
}

func (s *Store) AppendChatMessage(msg StoredChatMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`INSERT INTO chat_history (id, agent_id, role, content, tool_calls, tool_call_id, tokens, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.AgentID, msg.Role, msg.Content, msg.ToolCalls, msg.ToolCallID, msg.Tokens, msg.Metadata)
	if err != nil {
		return fmt.Errorf("append chat message: %w", err)
	}
	return nil
}

func (s *Store) LoadChatHistory(agentID string, limit int) ([]StoredChatMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var query string
	var args []interface{}

	if limit > 0 {
		// Sub-select to get the last N rows, then re-order chronologically
		query = `SELECT id, agent_id, role, content, tool_calls, tool_call_id, tokens, metadata, created_at FROM (
			SELECT id, agent_id, role, content, tool_calls, tool_call_id, tokens, metadata, created_at
			FROM chat_history WHERE agent_id = ? ORDER BY created_at DESC LIMIT ?
		) sub ORDER BY created_at ASC`
		args = []interface{}{agentID, limit}
	} else {
		query = `SELECT id, agent_id, role, content, tool_calls, tool_call_id, tokens, metadata, created_at FROM chat_history WHERE agent_id = ? ORDER BY created_at ASC`
		args = []interface{}{agentID}
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("load chat history: %w", err)
	}
	defer rows.Close()

	var messages []StoredChatMessage
	for rows.Next() {
		var m StoredChatMessage
		if err := rows.Scan(&m.ID, &m.AgentID, &m.Role, &m.Content, &m.ToolCalls, &m.ToolCallID, &m.Tokens, &m.Metadata, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan chat message: %w", err)
		}
		messages = append(messages, m)
	}
	return messages, nil
}

func (s *Store) ClearChatHistory(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`DELETE FROM chat_history WHERE agent_id = ?`, agentID)
	if err != nil {
		return fmt.Errorf("clear chat history: %w", err)
	}
	return nil
}

func (s *Store) ReplaceChatHistory(agentID string, messages []StoredChatMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin replace chat history tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`DELETE FROM chat_history WHERE agent_id = ?`, agentID)
	if err != nil {
		return fmt.Errorf("clear chat history in tx: %w", err)
	}

	stmt, err := tx.Prepare(`INSERT INTO chat_history (id, agent_id, role, content, tool_calls, tool_call_id, tokens, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare chat insert: %w", err)
	}
	defer stmt.Close()

	for _, m := range messages {
		_, err = stmt.Exec(m.ID, m.AgentID, m.Role, m.Content, m.ToolCalls, m.ToolCallID, m.Tokens, m.Metadata)
		if err != nil {
			return fmt.Errorf("insert chat message in tx: %w", err)
		}
	}

	return tx.Commit()
}

func (s *Store) GetChatTokenCount(agentID string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var total int64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(tokens), 0) FROM chat_history WHERE agent_id = ?`, agentID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("get chat token count: %w", err)
	}
	return total, nil
}
