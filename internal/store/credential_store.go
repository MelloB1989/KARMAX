package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Connector credentials.
//
// KARMAX owns these, not the connectors. Every integration that stores its own
// tokens invents its own refresh, its own expiry handling and its own file
// permissions, and most get at least one of the three wrong.

// Credential is one connector's stored configuration and tokens.
type Credential struct {
	Connector    string
	Config       map[string]string
	AccessToken  string
	RefreshToken string
	ExpiresAt    *time.Time
	Enabled      bool
	UpdatedAt    time.Time
}

// SaveCredential stores or replaces a connector's credentials.
func (s *Store) SaveCredential(c Credential) error {
	cfg, err := json.Marshal(c.Config)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(`
INSERT INTO connector_credentials (connector, config, access_token, refresh_token, expires_at, enabled, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(connector) DO UPDATE SET
  config = excluded.config, access_token = excluded.access_token,
  refresh_token = excluded.refresh_token, expires_at = excluded.expires_at,
  enabled = excluded.enabled, updated_at = excluded.updated_at`,
		c.Connector, string(cfg), c.AccessToken, c.RefreshToken, c.ExpiresAt, c.Enabled, time.Now())
	return err
}

// SaveTokens updates only the token half, for a refresh.
func (s *Store) SaveTokens(connector, access, refresh string, expiresAt *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
UPDATE connector_credentials
SET access_token = ?, refresh_token = COALESCE(NULLIF(?, ''), refresh_token),
    expires_at = ?, updated_at = ?
WHERE connector = ?`, access, refresh, expiresAt, time.Now(), connector)
	return err
}

// Credential returns one connector's credentials.
func (s *Store) Credential(connector string) (*Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var (
		c       Credential
		cfg     string
		expires sql.NullTime
	)
	err := s.db.QueryRow(`
SELECT connector, config, access_token, refresh_token, expires_at, enabled, updated_at
FROM connector_credentials WHERE connector = ?`, connector).
		Scan(&c.Connector, &cfg, &c.AccessToken, &c.RefreshToken, &expires, &c.Enabled, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing stored is the normal state of an integration nobody has
		// logged into yet, not a failure. Returning ErrNoRows made every
		// unconfigured integration report a database error instead of "not
		// connected", and hid a real read failure among them.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(cfg), &c.Config)
	if expires.Valid {
		at := expires.Time
		c.ExpiresAt = &at
	}
	return &c, nil
}

// EnabledConnectors lists the connectors that are configured and turned on.
func (s *Store) EnabledConnectors() ([]Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
SELECT connector, config, access_token, refresh_token, expires_at, enabled, updated_at
FROM connector_credentials WHERE enabled = 1 ORDER BY connector`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Credential
	for rows.Next() {
		var (
			c       Credential
			cfg     string
			expires sql.NullTime
		)
		if err := rows.Scan(&c.Connector, &cfg, &c.AccessToken, &c.RefreshToken,
			&expires, &c.Enabled, &c.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(cfg), &c.Config)
		if expires.Valid {
			at := expires.Time
			c.ExpiresAt = &at
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetConnectorEnabled turns a connector on or off without losing its setup.
func (s *Store) SetConnectorEnabled(connector string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`UPDATE connector_credentials SET enabled = ?, updated_at = ? WHERE connector = ?`,
		enabled, time.Now(), connector)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteCredential removes a connector's setup entirely.
func (s *Store) DeleteCredential(connector string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM connector_credentials WHERE connector = ?`, connector)
	return err
}

// SourceCursor is where a polling source got to.
func (s *Store) SourceCursor(connector, source string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var cursor string
	err := s.db.QueryRow(
		`SELECT cursor FROM connector_cursors WHERE connector = ? AND source = ?`,
		connector, source).Scan(&cursor)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return cursor, err
}

// SetSourceCursor records where a polling source got to, so a restart neither
// re-delivers nor skips.
func (s *Store) SetSourceCursor(connector, source, cursor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
INSERT INTO connector_cursors (connector, source, cursor, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT(connector, source) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`,
		connector, source, cursor, time.Now())
	return err
}
