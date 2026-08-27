package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Console operators: the humans who can sign into the web console.
//
// Kept apart from `directory`, which maps external identities onto org
// members. Everyone in the directory has been *seen*; only the people here can
// log in. Conflating the two would mean anyone who ever posted in a connected
// Slack could reach the console.

// ConsoleUser is one console account. PasswordHash never leaves this package.
type ConsoleUser struct {
	Member    string
	Name      string
	Role      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ConsoleSession is a live login.
type ConsoleSession struct {
	Token     string
	Member    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ConsoleRoles are the roles the console recognises, weakest first.
var ConsoleRoles = []string{"viewer", "operator", "admin"}

// ValidConsoleRole reports whether role is one the console understands. An
// unrecognised role is rejected rather than stored: a typo'd "admn" that falls
// through to a default would be a silent privilege decision.
func ValidConsoleRole(role string) bool {
	for _, r := range ConsoleRoles {
		if r == role {
			return true
		}
	}
	return false
}

// ErrConsoleUserExists is returned when bootstrapping over an existing account.
var ErrConsoleUserExists = errors.New("console user already exists")

// CountConsoleUsers reports how many console accounts exist. Zero means the
// install has never been bootstrapped.
func (s *Store) CountConsoleUsers() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var n int
	err := s.queryRow(`SELECT COUNT(*) FROM console_users`).Scan(&n)
	return n, err
}

// CreateConsoleUser adds an account, hashing the password with bcrypt.
func (s *Store) CreateConsoleUser(member, name, role, password string) (ConsoleUser, error) {
	member = strings.TrimSpace(member)
	if member == "" {
		return ConsoleUser{}, errors.New("member is required")
	}
	if len(password) < 8 {
		return ConsoleUser{}, errors.New("password must be at least 8 characters")
	}
	if !ValidConsoleRole(role) {
		return ConsoleUser{}, fmt.Errorf("unknown role %q", role)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return ConsoleUser{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// INSERT, not upsert: bootstrapping twice must fail loudly rather than
	// quietly reset the admin's password.
	if _, err := s.exec(`
INSERT INTO console_users (member, name, role, password_hash, created_at, updated_at)
VALUES (?, ?, ?, ?, datetime('now'), datetime('now'))`,
		member, name, role, string(hash)); err != nil {
		if isUniqueViolation(err) {
			return ConsoleUser{}, ErrConsoleUserExists
		}
		return ConsoleUser{}, err
	}
	return ConsoleUser{Member: member, Name: name, Role: role}, nil
}

// AuthenticateConsoleUser checks a password and returns the account.
//
// A missing user and a wrong password are the same error on purpose: telling
// them apart lets anyone enumerate who has an account.
func (s *Store) AuthenticateConsoleUser(member, password string) (ConsoleUser, error) {
	s.mu.RLock()
	var u ConsoleUser
	var hash string
	err := s.queryRow(`
SELECT member, name, role, password_hash FROM console_users WHERE member = ?`, member).
		Scan(&u.Member, &u.Name, &u.Role, &hash)
	s.mu.RUnlock()

	if err == sql.ErrNoRows {
		// Spend the time anyway. Returning early on an unknown user makes the
		// two cases distinguishable by a stopwatch.
		bcrypt.CompareHashAndPassword([]byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinvalidinv"), []byte(password))
		return ConsoleUser{}, errors.New("invalid credentials")
	}
	if err != nil {
		return ConsoleUser{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return ConsoleUser{}, errors.New("invalid credentials")
	}
	return u, nil
}

// ConsoleUserByMember reads one account.
func (s *Store) ConsoleUserByMember(member string) (ConsoleUser, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var u ConsoleUser
	err := s.queryRow(`SELECT member, name, role FROM console_users WHERE member = ?`, member).
		Scan(&u.Member, &u.Name, &u.Role)
	if err == sql.ErrNoRows {
		return ConsoleUser{}, false, nil
	}
	if err != nil {
		return ConsoleUser{}, false, err
	}
	return u, true, nil
}

// CreateConsoleSession issues a session token valid for ttl.
func (s *Store) CreateConsoleSession(member string, ttl time.Duration) (ConsoleSession, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ConsoleSession{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().UTC().Add(ttl)

	s.mu.Lock()
	defer s.mu.Unlock()

	// The expiry is written as a formatted string, not a time.Time: the column
	// is compared against datetime('now') in SQL, and SQLite compares those as
	// text.
	if _, err := s.exec(`
INSERT INTO console_sessions (token, member, created_at, expires_at)
VALUES (?, ?, datetime('now'), ?)`,
		token, member, expires.Format("2006-01-02 15:04:05")); err != nil {
		return ConsoleSession{}, err
	}
	return ConsoleSession{Token: token, Member: member, ExpiresAt: expires}, nil
}

// ConsoleSessionUser resolves a session token to its account, or reports that
// it is unknown or expired.
func (s *Store) ConsoleSessionUser(token string) (ConsoleUser, bool, error) {
	if token == "" {
		return ConsoleUser{}, false, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var u ConsoleUser
	err := s.queryRow(`
SELECT u.member, u.name, u.role
FROM console_sessions s JOIN console_users u ON u.member = s.member
WHERE s.token = ? AND s.expires_at > datetime('now')`, token).
		Scan(&u.Member, &u.Name, &u.Role)
	if err == sql.ErrNoRows {
		return ConsoleUser{}, false, nil
	}
	if err != nil {
		return ConsoleUser{}, false, err
	}
	return u, true, nil
}

// DeleteConsoleSession revokes one token. Logging out has to actually revoke,
// which is the whole reason sessions are rows and not signed tokens.
func (s *Store) DeleteConsoleSession(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`DELETE FROM console_sessions WHERE token = ?`, token)
	return err
}

// PurgeExpiredConsoleSessions drops sessions that have lapsed.
func (s *Store) PurgeExpiredConsoleSessions() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.exec(`DELETE FROM console_sessions WHERE expires_at <= datetime('now')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetConsoleRole assigns a role to a member who may have no account yet.
func (s *Store) SetConsoleRole(member, role string) error {
	if !ValidConsoleRole(role) {
		return fmt.Errorf("unknown role %q", role)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.exec(`
INSERT INTO console_roles (member, role, updated_at) VALUES (?, ?, datetime('now'))
ON CONFLICT(member) DO UPDATE SET role = excluded.role, updated_at = excluded.updated_at`,
		member, role); err != nil {
		return err
	}
	// An existing account is the authority on its own role, so keep the two in
	// step rather than leaving the console reading one and auth the other.
	_, err := s.exec(`UPDATE console_users SET role = ?, updated_at = datetime('now') WHERE member = ?`, role, member)
	return err
}

// ConsoleRoleAssignments returns every explicitly assigned role.
func (s *Store) ConsoleRoleAssignments() (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`SELECT member, role FROM console_roles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var member, role string
		if err := rows.Scan(&member, &role); err != nil {
			return nil, err
		}
		out[member] = role
	}
	return out, rows.Err()
}

// ListConsoleUsers returns every console account, weakest ordering by member.
func (s *Store) ListConsoleUsers() ([]ConsoleUser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`SELECT member, name, role FROM console_users ORDER BY member`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConsoleUser
	for rows.Next() {
		var u ConsoleUser
		if err := rows.Scan(&u.Member, &u.Name, &u.Role); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// isUniqueViolation reports whether err is a duplicate-key error. Each driver
// words it differently, so this matches on the shared substrings rather than
// importing three driver packages for their error types.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || // sqlite, postgres
		strings.Contains(msg, "duplicate") // mysql
}

// UpdateConsoleUser changes an account's display name.
func (s *Store) UpdateConsoleUser(member, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.exec(`UPDATE console_users SET name = ?, updated_at = datetime('now') WHERE member = ?`,
		name, member)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such console user")
	}
	return nil
}

// SetConsolePassword replaces an account's password.
//
// Every session that account holds is revoked at the same time. A password
// change that leaves the old sessions alive does not lock anybody out, which is
// the entire reason people change passwords in a hurry.
func (s *Store) SetConsolePassword(member, password string) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.exec(`UPDATE console_users SET password_hash = ?, updated_at = datetime('now') WHERE member = ?`,
		string(hash), member)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such console user")
	}
	_, err = s.exec(`DELETE FROM console_sessions WHERE member = ?`, member)
	return err
}

// DeleteConsoleUser removes an account and every session it holds.
func (s *Store) DeleteConsoleUser(member string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.exec(`DELETE FROM console_sessions WHERE member = ?`, member); err != nil {
		return err
	}
	_, err := s.exec(`DELETE FROM console_users WHERE member = ?`, member)
	return err
}

// CountConsoleAdmins reports how many admin accounts exist.
//
// Used to refuse the last one being removed or demoted: an install with no
// admin has nobody who can appoint one, and the only way back is editing the
// database by hand.
func (s *Store) CountConsoleAdmins() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	err := s.queryRow(`SELECT COUNT(*) FROM console_users WHERE role = 'admin'`).Scan(&n)
	return n, err
}
