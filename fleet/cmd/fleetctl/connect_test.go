package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// tailnetFleetd answers as fleetd's tailnet listener: it knows who you are
// without a token, and records whether one was sent.
type tailnetFleetd struct {
	mu     sync.Mutex
	authed []string
}

func (f *tailnetFleetd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.authed = append(f.authed, r.URL.Path+" "+r.Header.Get("Authorization"))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/whoami":
		_, _ = io.WriteString(w, `{"scope":"member","name":"ram@example.com"}`)
	case "/v1/tasks":
		_, _ = io.WriteString(w, `[]`)
	default:
		_, _ = io.WriteString(w, `{"ok":true}`)
	}
}

// On a laptop on the tailnet: connect once, and every command after goes to
// that fleetd, with no token.
func TestConnectSavesTheFleetdEveryCommandThenUses(t *testing.T) {
	f := &tailnetFleetd{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	dir := t.TempDir()
	t.Setenv("FLEETD_URL", "")
	t.Setenv("FLEET_TOKEN", "")
	t.Setenv("FLEET_CONFIG", filepath.Join(dir, "none.yaml")) // not a fleet host
	t.Setenv("FLEETCTL_SERVER_FILE", filepath.Join(dir, "cfg", "server"))

	if err := run("tasks", nil); err == nil || !strings.Contains(err.Error(), "fleetctl connect") {
		t.Fatalf("before connect: %v", err)
	}
	out := captureStdout(t, func() error { return run("connect", []string{srv.URL + "/"}) })
	if !strings.Contains(out, "as ram@example.com: you run tasks as yourself") {
		t.Fatalf("connect said %q", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cfg", "server")); strings.TrimSpace(string(b)) != srv.URL {
		t.Fatalf("saved %q", b)
	}
	captureStdout(t, func() error { return run("tasks", []string{"--json"}) })
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.authed) != 2 || f.authed[0] != "/v1/whoami " || f.authed[1] != "/v1/tasks " {
		t.Fatalf("requests (path, Authorization) = %q", f.authed)
	}
}

func TestConnectToNothingSavesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEETCTL_SERVER_FILE", filepath.Join(dir, "server"))
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if err := run("connect", []string{url}); err == nil {
		t.Fatal("connected to nothing")
	}
	if _, err := os.Stat(filepath.Join(dir, "server")); !os.IsNotExist(err) {
		t.Fatalf("saved: %v", err)
	}
}

// captureStdout runs fn and returns what it printed; its error fails the test.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
