package store

import (
	"database/sql"
	"fmt"
	"time"
)

type StoredCodingSession struct {
	ID          string
	ToolType    string
	SessionID   string
	Description string
	Status      string
	AgentID     string
	Output      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (s *Store) SaveCodingSession(cs StoredCodingSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO coding_sessions (id, tool_type, session_id, description, status, agent_id, output, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			tool_type=excluded.tool_type,
			session_id=excluded.session_id,
			description=excluded.description,
			status=excluded.status,
			agent_id=excluded.agent_id,
			output=excluded.output,
			updated_at=datetime('now')`,
		cs.ID, cs.ToolType, cs.SessionID, cs.Description, cs.Status, cs.AgentID, cs.Output)
	if err != nil {
		return fmt.Errorf("save coding session: %w", err)
	}
	return nil
}

func (s *Store) ListCodingSessions(agentID string) ([]StoredCodingSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id, tool_type, session_id, description, status, agent_id, output, created_at, updated_at FROM coding_sessions WHERE agent_id = ? ORDER BY created_at DESC`, agentID)
	if err != nil {
		return nil, fmt.Errorf("list coding sessions: %w", err)
	}
	defer rows.Close()

	var sessions []StoredCodingSession
	for rows.Next() {
		var cs StoredCodingSession
		if err := rows.Scan(&cs.ID, &cs.ToolType, &cs.SessionID, &cs.Description, &cs.Status, &cs.AgentID, &cs.Output, &cs.CreatedAt, &cs.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan coding session: %w", err)
		}
		sessions = append(sessions, cs)
	}
	return sessions, nil
}

func (s *Store) GetCodingSession(id string) (*StoredCodingSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var cs StoredCodingSession
	err := s.db.QueryRow(`SELECT id, tool_type, session_id, description, status, agent_id, output, created_at, updated_at FROM coding_sessions WHERE id = ?`, id).
		Scan(&cs.ID, &cs.ToolType, &cs.SessionID, &cs.Description, &cs.Status, &cs.AgentID, &cs.Output, &cs.CreatedAt, &cs.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get coding session: %w", err)
	}
	return &cs, nil
}

func (s *Store) UpdateCodingSessionStatus(id, status, output string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`UPDATE coding_sessions SET status = ?, output = ?, updated_at = datetime('now') WHERE id = ?`, status, output, id)
	if err != nil {
		return fmt.Errorf("update coding session status: %w", err)
	}
	return nil
}
