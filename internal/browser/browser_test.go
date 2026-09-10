package browser

import (
	"context"
	"os"
	"testing"
)

// Two URLs on the same site raise the tab that is already there.
//
// Without this, every press of "Connect Google" adds another consent tab to a
// window that already has one open at the step the person stopped at.
func TestSameOrigin(t *testing.T) {
	same := [][2]string{
		{"https://example.com/", "https://example.com/other"},
		{"https://Example.com/a?b=c", "https://example.com/"},
		{"http://127.0.0.1:9222/json", "http://127.0.0.1:9222/"},
	}
	for _, pair := range same {
		if !sameOrigin(pair[0], pair[1]) {
			t.Errorf("%q and %q should share an origin", pair[0], pair[1])
		}
	}
	different := [][2]string{
		{"https://example.com/", "https://accounts.example.com/"},
		{"https://example.com/", "http://example.com/"},
		{"https://example.com/", "https://example.com:8443/"},
		{"about:blank", "https://example.com/"},
		{"", "https://example.com/"},
	}
	for _, pair := range different {
		if sameOrigin(pair[0], pair[1]) {
			t.Errorf("%q and %q should not share an origin", pair[0], pair[1])
		}
	}
}

// Nothing running means nothing to hand a harness, and saying so beats handing
// out a configuration that points at a closed port.
func TestMCPConfigNeedsARunningBrowser(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.MCPConfigJSON(context.Background()); err != ErrNotRunning {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}

// A session that has never started is not running, and asking does not create
// the profile directory as a side effect.
func TestColdSessionIsNotRunning(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if s.Running(context.Background()) {
		t.Fatal("a session that was never started reported itself running")
	}
	if _, err := os.Stat(s.Profile()); !os.IsNotExist(err) {
		t.Fatalf("asking about the browser created %s", s.Profile())
	}
}

// The same data directory is the same window.
func TestSharedIsOnePerDirectory(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if Shared(a) != Shared(a) {
		t.Fatal("two sessions for one data directory; they would fight over the profile lock")
	}
	if Shared(a) == Shared(b) {
		t.Fatal("two data directories collapsed into one session")
	}
}
