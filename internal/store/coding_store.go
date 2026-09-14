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

	_, err := s.exec(`
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

// ListCodingSessions returns one agent's sessions, or — when agentID is empty
// — every agent's.
//
// The empty case is not a footgun waiting to happen: "what has been built
// lately" is a question about the person, not about which of their agents
// happened to run the task, and the previous behaviour of matching agent_id = ”
// silently returned nothing at all.
func (s *Store) ListCodingSessions(agentID string) ([]StoredCodingSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, tool_type, session_id, description, status, agent_id, output, created_at, updated_at FROM coding_sessions WHERE agent_id = ? ORDER BY created_at DESC`
	args := []any{agentID}
	if agentID == "" {
		query = `SELECT id, tool_type, session_id, description, status, agent_id, output, created_at, updated_at FROM coding_sessions ORDER BY created_at DESC`
		args = nil
	}

	rows, err := s.query(query, args...)
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
	err := s.queryRow(`SELECT id, tool_type, session_id, description, status, agent_id, output, created_at, updated_at FROM coding_sessions WHERE id = ?`, id).
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

	_, err := s.exec(`UPDATE coding_sessions SET status = ?, output = ?, updated_at = datetime('now') WHERE id = ?`, status, output, id)
	if err != nil {
		return fmt.Errorf("update coding session status: %w", err)
	}
	return nil
}

// DeleteCodingSessionsBySessionID removes every row for one session_id. A
// session resumed across several turns accumulates one row per turn under
// the same session_id (id is minted fresh each call), so this is the one
// delete that reclaims all of them.
func (s *Store) DeleteCodingSessionsBySessionID(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`DELETE FROM coding_sessions WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("delete coding sessions: %w", err)
	}
	return nil
}

// SaveSessionKey records that sessionUUID is the real Claude Code CLI
// session minted for the stable key a caller (the LYZN tasks recipe, keyed
// by task id) uses instead of a uuid of its own. Call this ONLY after the
// CLI has actually created that session — see ClaudeCodeTool.run — so a row
// here never outruns the session it claims to name.
func (s *Store) SaveSessionKey(key, sessionUUID, toolType string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.exec(`
		INSERT INTO coding_session_keys (session_key, session_uuid, tool_type, updated_at)
		VALUES (?, ?, ?, datetime('now'))
		ON CONFLICT(session_key) DO UPDATE SET
			session_uuid=excluded.session_uuid,
			tool_type=excluded.tool_type,
			updated_at=datetime('now')`,
		key, sessionUUID, toolType)
	if err != nil {
		return fmt.Errorf("save session key: %w", err)
	}
	return nil
}

// GetSessionKey resolves a stable key to the CLI session uuid last minted
// for it. Returns "" with no error when nothing has been minted yet — the
// ordinary state for a key's first turn, not a failure.
func (s *Store) GetSessionKey(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var sessionUUID string
	err := s.queryRow(`SELECT session_uuid FROM coding_session_keys WHERE session_key = ?`, key).Scan(&sessionUUID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get session key: %w", err)
	}
	return sessionUUID, nil
}

// DeleteSessionKey removes a key's mapping — the terminal cleanup alongside
// deleting the transcript it pointed at, and also how a stale mapping (the
// CLI reports "No conversation found" on --resume) is dropped so the next
// turn mints a fresh session instead of retrying the same dead one forever.
// Deleting a mapping that does not exist is not an error.
func (s *Store) DeleteSessionKey(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.exec(`DELETE FROM coding_session_keys WHERE session_key = ?`, key)
	if err != nil {
		return fmt.Errorf("delete session key: %w", err)
	}
	return nil
}

// ListStaleCodingSessionIDs returns the distinct session ids matching prefix
// whose most recent row was last touched before cutoff — the six-hour
// backstop's own query.
func (s *Store) ListStaleCodingSessionIDs(prefix string, cutoff time.Time) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.query(`SELECT DISTINCT session_id FROM coding_sessions WHERE session_id LIKE ? AND updated_at < ?`,
		prefix+"%", cutoff)
	if err != nil {
		return nil, fmt.Errorf("list stale coding sessions: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stale coding session: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
