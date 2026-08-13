package store

import (
	"database/sql"
	"errors"
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

// ConversationStats describes the stored conversation for one agent.
//
// Compaction was only ever visible by grepping the daemon log, so "is it even
// running?" had no answer short of reading source. These are the numbers that
// answer it: where the token count sits against the threshold, and when the last
// compaction actually rewrote the history.
type ConversationStats struct {
	Messages      int
	ByRole        map[string]int
	WithToolCalls int
	Tokens        int64
	// LastCompactedAt is the timestamp of the newest summary row. Compaction
	// writes exactly one, so its age is the age of the last compaction. Zero
	// means the history has never been compacted.
	LastCompactedAt time.Time
	OldestMessageAt time.Time
}

func (s *Store) ConversationStats(agentID string) (ConversationStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := ConversationStats{ByRole: map[string]int{}}

	rows, err := s.db.Query(`
		SELECT role, COUNT(*),
		       SUM(CASE WHEN tool_calls IS NOT NULL AND tool_calls NOT IN ('', 'null', '[]') THEN 1 ELSE 0 END)
		FROM chat_history WHERE agent_id = ? GROUP BY role`, agentID)
	if err != nil {
		return out, fmt.Errorf("conversation stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var role string
		var count, withTools int
		if err := rows.Scan(&role, &count, &withTools); err != nil {
			return out, fmt.Errorf("scan conversation stats: %w", err)
		}
		out.ByRole[role] = count
		out.Messages += count
		out.WithToolCalls += withTools
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("conversation stats: %w", err)
	}

	if err := s.db.QueryRow(`SELECT COALESCE(SUM(tokens), 0) FROM chat_history WHERE agent_id = ?`,
		agentID).Scan(&out.Tokens); err != nil {
		return out, fmt.Errorf("conversation tokens: %w", err)
	}

	// ORDER BY ... LIMIT 1 rather than MAX/MIN: an aggregate loses the column's
	// declared DATETIME type, so the driver hands back a string that will not
	// scan into a time and the timestamp silently reads as "never".
	out.LastCompactedAt, err = s.chatTimestamp(`
		SELECT created_at FROM chat_history
		WHERE agent_id = ? AND role = 'system' AND content LIKE '%Previous Conversation Summary%'
		ORDER BY created_at DESC LIMIT 1`, agentID)
	if err != nil {
		return out, err
	}
	out.OldestMessageAt, err = s.chatTimestamp(`
		SELECT created_at FROM chat_history WHERE agent_id = ? ORDER BY created_at ASC LIMIT 1`, agentID)
	if err != nil {
		return out, err
	}
	return out, nil
}

// chatTimestamp reads one timestamp, treating "no such row" as a zero time
// rather than an error — a history with no compaction yet is a normal state.
func (s *Store) chatTimestamp(query, agentID string) (time.Time, error) {
	var t sql.NullTime
	switch err := s.db.QueryRow(query, agentID).Scan(&t); {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, fmt.Errorf("conversation timestamp: %w", err)
	}
	if !t.Valid {
		return time.Time{}, nil
	}
	return t.Time, nil
}
