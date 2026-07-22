package store

import (
	"database/sql"
	"strings"
	"time"
)

// Short-term memory engine.
//
// Long-term memory (memory_entries) is curated, deduplicated, and meant to last.
// Loops also need the opposite: cheap scratch state scoped to a thing they're
// working on — "what did I just tell this chat", "this person asked me to stop",
// "the topic we're on" — that should expire on its own and never pollute
// long-term recall.
//
// This is that store: a durable key/value space partitioned into GROUPS. A loop
// chooses its own group naming (wa-monitor uses one group per chat) and its own
// keys/values. Every entry may carry a TTL; expired entries are invisible to
// reads and swept periodically, so callers never have to think about expiry.

// KVEntry is one short-term memory.
type KVEntry struct {
	Group     string     `json:"group"`
	Key       string     `json:"key"`
	Value     string     `json:"value"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// KVSet writes (or overwrites) a value. A ttl of 0 means "no expiry".
func (s *Store) KVSet(group, key, value string, ttl time.Duration) error {
	group, key = strings.TrimSpace(group), strings.TrimSpace(key)
	if group == "" || key == "" {
		return sql.ErrNoRows
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var expires any
	if ttl > 0 {
		expires = time.Now().Add(ttl).UTC().Format("2006-01-02 15:04:05")
	}
	_, err := s.db.Exec(`
INSERT INTO kv_memory (grp, key, value, expires_at, created_at, updated_at)
VALUES (?, ?, ?, ?, datetime('now'), datetime('now'))
ON CONFLICT(grp, key) DO UPDATE SET
	value = excluded.value,
	expires_at = excluded.expires_at,
	updated_at = datetime('now')`, group, key, value, expires)
	return err
}

// KVGet returns a value, or found=false when it's absent or expired.
func (s *Store) KVGet(group, key string) (value string, found bool, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRow(`
SELECT value FROM kv_memory
WHERE grp = ? AND key = ? AND (expires_at IS NULL OR expires_at > datetime('now'))`,
		strings.TrimSpace(group), strings.TrimSpace(key))
	if err := row.Scan(&value); err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, err
	}
	return value, true, nil
}

// KVList returns every live entry in a group, newest-updated first. This is what
// a loop injects as context.
func (s *Store) KVList(group string) ([]KVEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
SELECT grp, key, value, expires_at, updated_at FROM kv_memory
WHERE grp = ? AND (expires_at IS NULL OR expires_at > datetime('now'))
ORDER BY updated_at DESC`, strings.TrimSpace(group))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []KVEntry
	for rows.Next() {
		var e KVEntry
		var expires sql.NullTime
		var updated time.Time
		if err := rows.Scan(&e.Group, &e.Key, &e.Value, &expires, &updated); err != nil {
			return nil, err
		}
		if expires.Valid {
			t := expires.Time
			e.ExpiresAt = &t
		}
		e.UpdatedAt = updated
		out = append(out, e)
	}
	return out, rows.Err()
}

// KVDelete removes one key.
func (s *Store) KVDelete(group, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM kv_memory WHERE grp = ? AND key = ?`,
		strings.TrimSpace(group), strings.TrimSpace(key))
	return err
}

// KVClearGroup drops a whole group (e.g. a conversation that ended).
func (s *Store) KVClearGroup(group string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM kv_memory WHERE grp = ?`, strings.TrimSpace(group))
	return err
}

// KVPurgeExpired deletes entries whose TTL has passed and returns how many went.
// Reads already hide expired rows; this just keeps the table from growing.
func (s *Store) KVPurgeExpired() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM kv_memory WHERE expires_at IS NOT NULL AND expires_at <= datetime('now')`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
