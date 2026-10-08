package store

import "time"

// What System One decided about each event.
//
// Kept because thresholds are the whole risk surface of screening: a bar set
// too high wakes the brain for noise, one set too low loses work, and neither
// can be judged except against traffic that actually arrived. The probabilities
// are stored alongside the outcome so a threshold can be re-cut after the fact
// without having to re-run anything.

// ReflexVerdict is one recorded screening decision.
type ReflexVerdict struct {
	EventID   string
	EventKind string
	AgentID   string
	Action    string
	Verdict   string
	CreatedAt time.Time
}

// RecordReflexVerdict stores one decision. Re-screening an event replaces its
// row rather than accumulating duplicates.
func (s *Store) RecordReflexVerdict(eventID, eventKind, agentID, action, verdict string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.exec(`
		INSERT INTO reflex_verdicts (event_id, event_kind, agent_id, action, verdict, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(event_id) DO UPDATE SET
			event_kind=excluded.event_kind,
			agent_id=excluded.agent_id,
			action=excluded.action,
			verdict=excluded.verdict,
			created_at=excluded.created_at`,
		eventID, eventKind, agentID, action, verdict, time.Now())
	return err
}

// RecentReflexVerdicts returns the most recent decisions, newest first.
func (s *Store) RecentReflexVerdicts(limit int) ([]ReflexVerdict, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}
	rows, err := s.query(`
		SELECT event_id, event_kind, agent_id, action, verdict, created_at
		FROM reflex_verdicts ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ReflexVerdict
	for rows.Next() {
		var v ReflexVerdict
		if err := rows.Scan(&v.EventID, &v.EventKind, &v.AgentID, &v.Action, &v.Verdict, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ReflexActionCounts totals decisions by action since a point in time, which is
// the view a threshold is actually tuned from.
func (s *Store) ReflexActionCounts(since time.Time) (map[string]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
		SELECT action, COUNT(*) FROM reflex_verdicts
		WHERE created_at >= ? GROUP BY action`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var action string
		var n int
		if err := rows.Scan(&action, &n); err != nil {
			return nil, err
		}
		out[action] = n
	}
	return out, rows.Err()
}
