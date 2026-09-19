package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
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

// Stopping notifies a registered callback, so a long-lived harness session
// whose --mcp-config went stale can be recycled.
func TestStopNotifiesOnStateChange(t *testing.T) {
	s := New(t.TempDir())
	var got []bool
	s.OnStateChange(func(running bool) { got = append(got, running) })

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(got) != 1 || got[0] != false {
		t.Fatalf("notified %v, want [false]", got)
	}
}

// newSlowClosingDevTools fakes a DevTools endpoint that answers
// /json/version immediately (so alive() reports true) but only answers
// /json/close after closeDelay — standing in for a Chromium that takes a
// while to actually go away once asked. Each handler runs in its own
// goroutine (httptest's normal behaviour), so the slow /json/close does not
// block a concurrent /json/version probe.
func newSlowClosingDevTools(t *testing.T, closeDelay time.Duration) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"Browser": "fake/1.0"})
	})
	mux.HandleFunc("/json/close", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(closeDelay)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// Stop's own polite /json/close request can take a while to answer — real
// Chromium is in no hurry to shut down. Running()/Endpoint()/MCPConfigJSON()
// must stop advertising the browser as soon as Stop begins, not once that
// request finally returns: a session spawned in between would otherwise get
// a --mcp-config pointing at an endpoint already on its way out, which is
// exactly what spec 1 promises cannot happen.
func TestRunningFailsClosedAsSoonAsStopBegins(t *testing.T) {
	port := newSlowClosingDevTools(t, 300*time.Millisecond)
	s := New(t.TempDir())
	s.mu.Lock()
	s.port = port
	s.mu.Unlock()
	s.saveState(state{Port: port})

	ctx := context.Background()
	if !s.Running(ctx) {
		t.Fatal("precondition: session should report running before Stop is called")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.Stop(ctx)
	}()

	// Well before the fake /json/close's deliberate delay could have
	// elapsed, but comfortably after Stop has started.
	time.Sleep(50 * time.Millisecond)
	if s.Running(ctx) {
		t.Fatal("Running() still reported true while Stop was mid-flight, waiting on /json/close")
	}

	wg.Wait()
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
