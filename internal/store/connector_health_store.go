package store

import (
	"database/sql"
	"time"
)

// The last thing we learned about whether a connector works.
//
// Persisted rather than cached in memory because a restart is not new
// information: an integration that was healthy a minute before a deploy is
// almost certainly still healthy after it, and showing "unknown" for everything
// on every boot trains people to ignore the column.

// ConnectorHealth is one connector's last verdict.
type ConnectorHealth struct {
	Connector string
	Status    string // healthy | degraded | failed | not_configured
	Detail    string
	CheckedAt time.Time
}

// Stale reports whether a verdict is old enough that it should not be presented
// as current.
func (h ConnectorHealth) Stale(after time.Duration) bool {
	return h.CheckedAt.IsZero() || time.Since(h.CheckedAt) > after
}

// SaveConnectorHealth records a verdict.
func (s *Store) SaveConnectorHealth(h ConnectorHealth) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO connector_health (connector, status, detail, checked_at)
VALUES (?, ?, ?, datetime('now'))
ON CONFLICT(connector) DO UPDATE SET
  status = excluded.status, detail = excluded.detail, checked_at = excluded.checked_at`,
		h.Connector, h.Status, h.Detail)
	return err
}

// ConnectorHealthFor reads one connector's verdict, or nil when it has never
// been checked.
func (s *Store) ConnectorHealthFor(connector string) (*ConnectorHealth, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var h ConnectorHealth
	var checked sql.NullTime
	err := s.queryRow(`
SELECT connector, status, detail, checked_at FROM connector_health WHERE connector = ?`, connector).
		Scan(&h.Connector, &h.Status, &h.Detail, &checked)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if checked.Valid {
		h.CheckedAt = checked.Time
	}
	return &h, nil
}

// AllConnectorHealth reads every verdict at once, for a list view that would
// otherwise issue one query per connector.
func (s *Store) AllConnectorHealth() (map[string]ConnectorHealth, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`SELECT connector, status, detail, checked_at FROM connector_health`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]ConnectorHealth{}
	for rows.Next() {
		var h ConnectorHealth
		var checked sql.NullTime
		if err := rows.Scan(&h.Connector, &h.Status, &h.Detail, &checked); err != nil {
			return nil, err
		}
		if checked.Valid {
			h.CheckedAt = checked.Time
		}
		out[h.Connector] = h
	}
	return out, rows.Err()
}
