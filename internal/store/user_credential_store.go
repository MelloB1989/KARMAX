package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// Credentials held per employee rather than per install.
//
// connector_credentials answers "how does KARMAX talk to Slack" — one bot, one
// token, one answer for everybody. This answers "how does KARMAX read PRIYA's
// calendar", which has as many answers as there are employees. Conflating the
// two would mean one person's mailbox token serving everyone's requests, which
// is not a permissions bug to tighten later; it is the wrong answer returned
// confidently.

// UserCredential is one employee's authorisation for one connector.
type UserCredential struct {
	Connector    string
	Member       string
	Account      string // the external account connected, e.g. their email
	AccessToken  string
	RefreshToken string
	Scopes       []string
	ExpiresAt    *time.Time
	UpdatedAt    time.Time
}

// Expired reports whether the access token needs refreshing.
//
// Treated as expired a minute early on purpose: a token that dies mid-request
// produces a 401 that reads like a revoked grant rather than an expiry, and
// sends whoever is debugging it to the wrong place entirely.
func (c UserCredential) Expired() bool {
	if c.ExpiresAt == nil {
		return false
	}
	return time.Now().After(c.ExpiresAt.Add(-time.Minute))
}

// SaveUserCredential stores or updates one employee's authorisation.
func (s *Store) SaveUserCredential(c UserCredential) error {
	if strings.TrimSpace(c.Connector) == "" || strings.TrimSpace(c.Member) == "" {
		return errors.New("a user credential needs both a connector and a member")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var expires any
	if c.ExpiresAt != nil {
		expires = c.ExpiresAt.UTC().Format("2006-01-02 15:04:05")
	}

	// Google returns a refresh token on the FIRST consent only. Overwriting a
	// stored one with the empty string on a later re-consent silently converts
	// a durable connection into one that dies in an hour, so an empty incoming
	// refresh token keeps whatever is already there.
	_, err := s.exec(`
INSERT INTO connector_user_credentials
  (connector, member, account, access_token, refresh_token, scopes, expires_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'))
ON CONFLICT(connector, member) DO UPDATE SET
  account       = excluded.account,
  access_token  = excluded.access_token,
  refresh_token = CASE WHEN excluded.refresh_token = '' THEN connector_user_credentials.refresh_token
                       ELSE excluded.refresh_token END,
  scopes        = excluded.scopes,
  expires_at    = excluded.expires_at,
  updated_at    = excluded.updated_at`,
		c.Connector, c.Member, c.Account, c.AccessToken, c.RefreshToken,
		strings.Join(c.Scopes, " "), expires)
	return err
}

// UserCredential reads one employee's authorisation.
func (s *Store) UserCredential(connector, member string) (*UserCredential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var c UserCredential
	var scopes string
	var expires sql.NullTime
	err := s.queryRow(`
SELECT connector, member, account, access_token, refresh_token, scopes, expires_at, updated_at
FROM connector_user_credentials WHERE connector = ? AND member = ?`, connector, member).
		Scan(&c.Connector, &c.Member, &c.Account, &c.AccessToken, &c.RefreshToken,
			&scopes, &expires, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if scopes != "" {
		c.Scopes = strings.Fields(scopes)
	}
	if expires.Valid {
		t := expires.Time
		c.ExpiresAt = &t
	}
	return &c, nil
}

// ListUserCredentials returns everyone who has connected one connector.
//
// Tokens are deliberately blanked: this feeds a "who has connected" list in the
// console, and a screen that only needs to show names should not be handed
// everybody's refresh tokens to leak.
func (s *Store) ListUserCredentials(connector string) ([]UserCredential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT member, account, scopes, expires_at, updated_at
FROM connector_user_credentials WHERE connector = ? ORDER BY member`, connector)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UserCredential
	for rows.Next() {
		c := UserCredential{Connector: connector}
		var scopes string
		var expires sql.NullTime
		if err := rows.Scan(&c.Member, &c.Account, &scopes, &expires, &c.UpdatedAt); err != nil {
			return nil, err
		}
		if scopes != "" {
			c.Scopes = strings.Fields(scopes)
		}
		if expires.Valid {
			t := expires.Time
			c.ExpiresAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteUserCredential disconnects one employee's account.
func (s *Store) DeleteUserCredential(connector, member string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`DELETE FROM connector_user_credentials WHERE connector = ? AND member = ?`,
		connector, member)
	return err
}

// --- pending OAuth authorisations -----------------------------------------

// OAuthState is a pending authorisation waiting for its callback.
type OAuthState struct {
	State     string
	Connector string
	Member    string
	Verifier  string
	Redirect  string
}

// CreateOAuthState records a pending authorisation and returns its state token.
func (s *Store) CreateOAuthState(connector, member, verifier, redirect string, ttl time.Duration) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	state := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.exec(`
INSERT INTO oauth_states (state, connector, member, verifier, redirect, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, datetime('now'), ?)`,
		state, connector, member, verifier, redirect,
		time.Now().UTC().Add(ttl).Format("2006-01-02 15:04:05"))
	if err != nil {
		return "", err
	}
	return state, nil
}

// RedeemOAuthState consumes a pending authorisation, exactly once.
//
// The row is deleted whether or not it had expired: a state token is a
// single-use credential, and leaving a redeemed one behind is what makes a
// leaked callback URL replayable.
func (s *Store) RedeemOAuthState(state string) (*OAuthState, error) {
	if strings.TrimSpace(state) == "" {
		return nil, errors.New("no state supplied")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var out OAuthState
	var expires time.Time
	err := s.queryRow(`
SELECT state, connector, member, verifier, redirect, expires_at FROM oauth_states WHERE state = ?`, state).
		Scan(&out.State, &out.Connector, &out.Member, &out.Verifier, &out.Redirect, &expires)
	if err == sql.ErrNoRows {
		return nil, errors.New("this authorisation link is unknown or has already been used")
	}
	if err != nil {
		return nil, err
	}

	if _, derr := s.exec(`DELETE FROM oauth_states WHERE state = ?`, state); derr != nil {
		return nil, derr
	}
	if time.Now().After(expires) {
		return nil, errors.New("this authorisation link has expired — start again from the console")
	}
	return &out, nil
}

// PurgeExpiredOAuthStates drops abandoned authorisations.
func (s *Store) PurgeExpiredOAuthStates() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.exec(`DELETE FROM oauth_states WHERE expires_at <= datetime('now')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
