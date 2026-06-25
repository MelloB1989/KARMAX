package store

import (
	"database/sql"
	"time"
)

// StoredDeviceAction is an action the phone app performs on-device (e.g. create
// a Calendar event or a Reminder via EventKit), enqueued by KARMAX.
type StoredDeviceAction struct {
	ID        string
	AgentID   string
	Kind      string // calendar_event | reminder
	Payload   string // JSON spec
	Status    string // pending | done | failed
	Result    string
	CreatedAt time.Time
	DoneAt    *time.Time
}

func (s *Store) CreateDeviceAction(a StoredDeviceAction) error {
	if a.Status == "" {
		a.Status = "pending"
	}
	_, err := s.db.Exec(
		`INSERT INTO device_actions (id, agent_id, kind, payload, status) VALUES (?, ?, ?, ?, ?)`,
		a.ID, a.AgentID, a.Kind, a.Payload, a.Status,
	)
	return err
}

func (s *Store) ListDeviceActions(status string, limit int) ([]StoredDeviceAction, error) {
	if limit <= 0 {
		limit = 50
	}
	const cols = `id, agent_id, kind, payload, status, COALESCE(result,''), created_at, done_at`
	var (
		rows *sql.Rows
		err  error
	)
	if status != "" {
		rows, err = s.db.Query(`SELECT `+cols+` FROM device_actions WHERE status = ? ORDER BY created_at DESC LIMIT ?`, status, limit)
	} else {
		rows, err = s.db.Query(`SELECT `+cols+` FROM device_actions ORDER BY created_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StoredDeviceAction
	for rows.Next() {
		var a StoredDeviceAction
		var done sql.NullTime
		if err := rows.Scan(&a.ID, &a.AgentID, &a.Kind, &a.Payload, &a.Status, &a.Result, &a.CreatedAt, &done); err != nil {
			return nil, err
		}
		if done.Valid {
			t := done.Time
			a.DoneAt = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) CompleteDeviceAction(id, status, result string) error {
	_, err := s.db.Exec(
		`UPDATE device_actions SET status = ?, result = ?, done_at = datetime('now') WHERE id = ?`,
		status, result, id,
	)
	return err
}
