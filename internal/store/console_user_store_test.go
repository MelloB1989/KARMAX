package store

import (
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

func consoleStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "c.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBootstrapIsOnlyPossibleOnce(t *testing.T) {
	s := consoleStore(t)

	n, err := s.CountConsoleUsers()
	if err != nil || n != 0 {
		t.Fatalf("fresh install should have no users, got %d (%v)", n, err)
	}
	if _, err := s.CreateConsoleUser("nikhil", "Nikhil", "admin", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateConsoleUser("nikhil", "Someone Else", "admin", "another-pass"); err != ErrConsoleUserExists {
		t.Errorf("a second bootstrap must not silently reset the password, got %v", err)
	}
}

func TestAWrongPasswordIsRejected(t *testing.T) {
	s := consoleStore(t)
	if _, err := s.CreateConsoleUser("nikhil", "Nikhil", "admin", "correct-horse"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.AuthenticateConsoleUser("nikhil", "correct-horse"); err != nil {
		t.Errorf("the right password was rejected: %v", err)
	}
	if _, err := s.AuthenticateConsoleUser("nikhil", "wrong"); err == nil {
		t.Error("a wrong password was accepted")
	}
	// An unknown user must look exactly like a wrong password, or the error
	// tells an attacker which members exist.
	unknown, _ := s.AuthenticateConsoleUser("ghost", "wrong")
	if unknown.Member != "" {
		t.Error("an unknown user authenticated")
	}
}

func TestAShortPasswordIsRefused(t *testing.T) {
	s := consoleStore(t)
	if _, err := s.CreateConsoleUser("nikhil", "Nikhil", "admin", "short"); err == nil {
		t.Error("an 5-character password was accepted")
	}
}

func TestLogoutRevokesTheSession(t *testing.T) {
	s := consoleStore(t)
	if _, err := s.CreateConsoleUser("nikhil", "Nikhil", "admin", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	sess, err := s.CreateConsoleSession("nikhil", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := s.ConsoleSessionUser(sess.Token); !ok {
		t.Fatal("a fresh session did not resolve")
	}
	if err := s.DeleteConsoleSession(sess.Token); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ConsoleSessionUser(sess.Token); ok {
		t.Error("the session survived logout — revocation is the whole point of storing sessions as rows")
	}
}

// An expiry compared in SQL has to be written in the format the comparison
// uses; a time.Time here silently never expires under SQLite's text compare.
func TestAnExpiredSessionDoesNotResolve(t *testing.T) {
	s := consoleStore(t)
	if _, err := s.CreateConsoleUser("nikhil", "Nikhil", "admin", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	sess, err := s.CreateConsoleSession("nikhil", -time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := s.ConsoleSessionUser(sess.Token); ok {
		t.Error("an already-expired session resolved")
	}
	n, err := s.PurgeExpiredConsoleSessions()
	if err != nil || n != 1 {
		t.Errorf("purge should have removed 1 session, removed %d (%v)", n, err)
	}
}

func TestAnUnknownRoleIsRejected(t *testing.T) {
	s := consoleStore(t)
	if _, err := s.CreateConsoleUser("nikhil", "Nikhil", "admn", "correct-horse"); err == nil {
		t.Error("a typo'd role was stored")
	}
	if err := s.SetConsoleRole("someone", "superuser"); err == nil {
		t.Error("an unknown role was assigned")
	}
}

// A role change has to reach the account too, or auth reads one value and the
// settings screen displays another.
func TestSettingARoleUpdatesAnExistingAccount(t *testing.T) {
	s := consoleStore(t)
	if _, err := s.CreateConsoleUser("dev", "Dev", "viewer", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConsoleRole("dev", "operator"); err != nil {
		t.Fatal(err)
	}

	u, ok, err := s.ConsoleUserByMember("dev")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if u.Role != "operator" {
		t.Errorf("account role is %q, settings would show operator", u.Role)
	}
}
