package store

import (
	"encoding/json"
	"fmt"
	"time"
)

type AgentSnapshot struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	Restarts  int        `json:"restarts"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	LastEvent *time.Time `json:"last_event,omitempty"`
	LastErr   string     `json:"last_err,omitempty"`
	DefJSON   string     `json:"def_json"`
	StateJSON string     `json:"state_json,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (s *Store) SaveAgentSnapshot(snap AgentSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO agent_snapshots (id, name, status, restarts, started_at, last_event, last_err, def_json, state_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, status=excluded.status, restarts=excluded.restarts,
			started_at=excluded.started_at, last_event=excluded.last_event,
			last_err=excluded.last_err, def_json=excluded.def_json,
			state_json=excluded.state_json, updated_at=datetime('now')`,
		snap.ID, snap.Name, snap.Status, snap.Restarts, snap.StartedAt, snap.LastEvent, snap.LastErr, snap.DefJSON, snap.StateJSON,
	)
	return err
}

func (s *Store) GetAgentSnapshot(id string) (*AgentSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRow(`SELECT id, name, status, restarts, started_at, last_event, last_err, def_json, state_json, updated_at FROM agent_snapshots WHERE id = ?`, id)
	var snap AgentSnapshot
	err := row.Scan(&snap.ID, &snap.Name, &snap.Status, &snap.Restarts, &snap.StartedAt, &snap.LastEvent, &snap.LastErr, &snap.DefJSON, &snap.StateJSON, &snap.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *Store) ListAgentSnapshots() ([]AgentSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id, name, status, restarts, started_at, last_event, last_err, def_json, state_json, updated_at FROM agent_snapshots ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []AgentSnapshot
	for rows.Next() {
		var snap AgentSnapshot
		if err := rows.Scan(&snap.ID, &snap.Name, &snap.Status, &snap.Restarts, &snap.StartedAt, &snap.LastEvent, &snap.LastErr, &snap.DefJSON, &snap.StateJSON, &snap.UpdatedAt); err != nil {
			return nil, err
		}
		snaps = append(snaps, snap)
	}
	return snaps, nil
}

func (s *Store) DeleteAgentSnapshot(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM agent_snapshots WHERE id = ?`, id)
	return err
}

func (s *Store) SaveAgentState(id string, state any) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal agent state: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(`UPDATE agent_snapshots SET state_json = ?, updated_at = datetime('now') WHERE id = ?`, string(data), id)
	return err
}

func (s *Store) LoadAgentState(id string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var state string
	err := s.db.QueryRow(`SELECT COALESCE(state_json, '{}') FROM agent_snapshots WHERE id = ?`, id).Scan(&state)
	return state, err
}
