package store

import (
	"encoding/json"
	"time"
)

type StoredEvent struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	AgentID   string    `json:"agent_id,omitempty"`
	Payload   string    `json:"payload"`
	Meta      string    `json:"meta,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) AppendEvent(id, kind, agentID string, payload map[string]any, meta map[string]string) error {
	pJSON, _ := json.Marshal(payload)
	mJSON, _ := json.Marshal(meta)

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`INSERT INTO events (id, kind, agent_id, payload, meta) VALUES (?, ?, ?, ?, ?)`,
		id, kind, agentID, string(pJSON), string(mJSON))
	return err
}

func (s *Store) ListEvents(limit int, kindFilter string) ([]StoredEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, kind, agent_id, payload, meta, created_at FROM events`
	var args []any
	if kindFilter != "" {
		query += ` WHERE kind = ?`
		args = append(args, kindFilter)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []StoredEvent
	for rows.Next() {
		var e StoredEvent
		if err := rows.Scan(&e.ID, &e.Kind, &e.AgentID, &e.Payload, &e.Meta, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}

func (s *Store) ListEventsByAgent(agentID string, limit int) ([]StoredEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id, kind, agent_id, payload, meta, created_at FROM events WHERE agent_id = ? ORDER BY created_at DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []StoredEvent
	for rows.Next() {
		var e StoredEvent
		if err := rows.Scan(&e.ID, &e.Kind, &e.AgentID, &e.Payload, &e.Meta, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}
