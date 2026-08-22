package store

import (
	"database/sql"
	"strings"
	"time"
)

// Capability grants and their meter.
//
// Nothing has ambient authority: a loop, connector or peer can do exactly what
// it was granted and nothing else. The meter is aggregated per day rather than
// per call, because a row per tool call is a disk-fill bug, not an audit trail.

// Capability classes.
const (
	CapTool      = "tool"      // value: tool name, or a "prefix.*" pattern
	CapHTTP      = "http"      // value: hostname, or "*.example.com"
	CapMemory    = "memory"    // value: namespace, ":write" suffix for write access
	CapChannel   = "channel"   // value: comms channel id
	CapSpend     = "spend"     // value: a daily unit ceiling
	CapWildcard  = "*"         // value that grants everything in its class
	SubjectAny   = "*"         // a grant that applies to every subject
	writeSuffix  = ":write"    // memory grants distinguish read from write
	SpendUnitDay = "spend/day" // meter key for the daily spend ceiling
)

// Grant is one permission held by one subject.
type Grant struct {
	Subject    string // "loop:daily-digest", "peer:<key>", "connector:github"
	Capability string
	Value      string
	GrantedBy  string
	GrantedAt  time.Time
	ExpiresAt  *time.Time
}

// SaveGrant records a permission, replacing any identical one.
func (s *Store) SaveGrant(g Grant) error {
	if g.GrantedBy == "" {
		g.GrantedBy = "operator"
	}
	if g.GrantedAt.IsZero() {
		g.GrantedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO capability_grants (subject, capability, value, granted_by, granted_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(subject, capability, value) DO UPDATE SET
  granted_by = excluded.granted_by, granted_at = excluded.granted_at, expires_at = excluded.expires_at`,
		g.Subject, g.Capability, g.Value, g.GrantedBy, g.GrantedAt, g.ExpiresAt)
	return err
}

// RevokeGrant withdraws one permission.
func (s *Store) RevokeGrant(subject, capability, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(
		`DELETE FROM capability_grants WHERE subject = ? AND capability = ? AND value = ?`,
		subject, capability, value)
	return err
}

// RevokeSubject withdraws everything one subject holds, for uninstalling a loop
// or blocking a peer.
func (s *Store) RevokeSubject(subject string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.exec(`DELETE FROM capability_grants WHERE subject = ?`, subject)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Grants returns every unexpired permission a subject holds, plus the ones
// granted to every subject.
func (s *Store) Grants(subject string) ([]Grant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT subject, capability, value, granted_by, granted_at, expires_at
FROM capability_grants
WHERE (subject = ? OR subject = ?) AND (expires_at IS NULL OR expires_at > ?)
ORDER BY capability, value`, subject, SubjectAny, time.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGrants(rows)
}

// AllGrants lists every permission, for the operator's review.
func (s *Store) AllGrants() ([]Grant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT subject, capability, value, granted_by, granted_at, expires_at
FROM capability_grants ORDER BY subject, capability, value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGrants(rows)
}

func scanGrants(rows *sql.Rows) ([]Grant, error) {
	var out []Grant
	for rows.Next() {
		var g Grant
		var expires sql.NullTime
		if err := rows.Scan(&g.Subject, &g.Capability, &g.Value, &g.GrantedBy, &g.GrantedAt, &expires); err != nil {
			return nil, err
		}
		if expires.Valid {
			at := expires.Time
			g.ExpiresAt = &at
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MeterCapability records that a subject used, or was refused, a capability.
// Aggregated per day so this stays a report rather than a log.
func (s *Store) MeterCapability(subject, capability string, allowed bool, units int64) error {
	day := time.Now().UTC().Format("2006-01-02")
	allow, refuse := int64(0), int64(1)
	if allowed {
		allow, refuse = 1, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO capability_meter (subject, capability, day, allowed, refused, units)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(subject, capability, day) DO UPDATE SET
  allowed = capability_meter.allowed + excluded.allowed,
  refused = capability_meter.refused + excluded.refused,
  units   = capability_meter.units   + excluded.units`,
		subject, capability, day, allow, refuse, units)
	return err
}

// MeterReading is one subject's usage of one capability on one day.
type MeterReading struct {
	Subject    string
	Capability string
	Day        string
	Allowed    int64
	Refused    int64
	Units      int64
}

// UsageToday is what a subject has spent today, which is what a ceiling is
// checked against.
func (s *Store) UsageToday(subject, capability string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var units sql.NullInt64
	err := s.queryRow(`
SELECT units FROM capability_meter WHERE subject = ? AND capability = ? AND day = ?`,
		subject, capability, time.Now().UTC().Format("2006-01-02")).Scan(&units)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return units.Int64, err
}

// Meter returns recent readings, newest day first.
func (s *Store) Meter(since time.Time, limit int) ([]MeterReading, error) {
	if limit <= 0 {
		limit = 200
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT subject, capability, day, allowed, refused, units FROM capability_meter
WHERE day >= ? ORDER BY day DESC, subject, capability LIMIT ?`,
		since.UTC().Format("2006-01-02"), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MeterReading
	for rows.Next() {
		var m MeterReading
		if err := rows.Scan(&m.Subject, &m.Capability, &m.Day, &m.Allowed, &m.Refused, &m.Units); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PruneMeter drops readings older than before.
func (s *Store) PruneMeter(before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.exec(`DELETE FROM capability_meter WHERE day < ?`,
		before.UTC().Format("2006-01-02"))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MemoryValue renders a memory grant value, since read and write are different
// permissions over the same namespace.
func MemoryValue(namespace string, write bool) string {
	if write {
		return namespace + writeSuffix
	}
	return namespace
}

// SplitMemoryValue reverses MemoryValue.
func SplitMemoryValue(v string) (namespace string, write bool) {
	if strings.HasSuffix(v, writeSuffix) {
		return strings.TrimSuffix(v, writeSuffix), true
	}
	return v, false
}
