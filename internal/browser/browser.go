// Package browser is the operator's browser: one window they and the agent
// both use.
//
// Connecting anything that is not WhatsApp means signing in, and signing in
// means a browser. The old answer was to print a consent URL and hope somebody
// pasted it into the right profile; then, separately, to give the agent a
// browser of its own, logged into nothing. Two browsers, two sessions, and the
// agent could never see what the person had just signed into.
//
// So there is one. KARMAX launches a Chromium with a profile it owns, headed,
// with the DevTools protocol listening on loopback. The person signs into
// Google, Instagram, LinkedIn — whatever they want reachable — in that window.
// When an agent then needs the web, Playwright MCP attaches to the same browser
// over that endpoint and finds the sessions already there.
//
// The profile is KARMAX's own, never the person's daily Chrome profile. That is
// the boundary that makes this honest: what the agent can reach is exactly what
// the operator deliberately signed into here, and closing the session is one
// directory to delete.
//
// The DevTools endpoint is bound to 127.0.0.1. Any process running as this user
// can drive the browser through it — the same is true of the person's own
// keychain, and of every other CLI session on the box, but it is worth knowing
// rather than discovering.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/hostpaths"
)

// Session is the browser KARMAX runs. The zero value is not usable; call New.
type Session struct {
	dir string

	mu   sync.Mutex
	cmd  *exec.Cmd
	port int
}

// state is what survives a KARMAX restart, so a browser the person left open
// is found again rather than duplicated.
type state struct {
	Port int `json:"port"`
	PID  int `json:"pid"`
}

// New returns the session rooted at dir, which is created on first start.
//
// An empty dir falls back to ~/.karmax rather than to a relative path: a CLI
// invocation with no karmax.yaml to read would otherwise put a browser profile
// in whatever directory it happened to be run from, and find a different one
// the next time.
func New(dir string) *Session {
	if strings.TrimSpace(dir) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dir = filepath.Join(home, ".karmax")
	}
	return &Session{dir: filepath.Join(dir, "browser")}
}

// Endpoint is the DevTools base URL, or "" when nothing is listening.
func (s *Session) Endpoint(ctx context.Context) string {
	port := s.knownPort()
	if port == 0 || !alive(ctx, port) {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// Running reports whether the browser is up and answering.
func (s *Session) Running(ctx context.Context) bool { return s.Endpoint(ctx) != "" }

// Start launches the browser, or returns quietly if one is already up.
//
// It waits for the DevTools endpoint to answer rather than returning as soon as
// the process exists: a caller that immediately tries to open a tab would
// otherwise race the browser's startup and get a connection refused it cannot
// explain.
func (s *Session) Start(ctx context.Context) error {
	if s.Running(ctx) {
		return nil
	}

	bin := hostpaths.Browser()
	if bin == "" {
		return errors.New("no Chrome, Chromium or Edge on this machine — install one and try again")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("browser profile directory: %w", err)
	}

	port, err := freePort()
	if err != nil {
		return err
	}

	// --no-first-run and friends: this is a profile nobody has seen before, and
	// a first-run wizard over the top of it is one more thing between somebody
	// and signing into Gmail.
	cmd := exec.Command(bin,
		"--user-data-dir="+s.dir,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-allow-origins=*",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=Translate,MediaRouter",
		"about:blank",
	)
	cmd.Stdout, cmd.Stderr = nil, nil
	// Detached from KARMAX's own lifetime: restarting the daemon must not close
	// a window somebody is halfway through signing into.
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser: %w", err)
	}

	s.mu.Lock()
	s.cmd, s.port = cmd, port
	s.mu.Unlock()
	s.saveState(state{Port: port, PID: cmd.Process.Pid})

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if alive(ctx, port) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New("the browser started but never answered on its DevTools port")
}

// Tab is one open page.
type Tab struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
	Type  string `json:"type"`
}

// Tabs lists the pages currently open.
func (s *Session) Tabs(ctx context.Context) ([]Tab, error) {
	endpoint := s.Endpoint(ctx)
	if endpoint == "" {
		return nil, ErrNotRunning
	}
	var all []Tab
	if err := getJSON(ctx, endpoint+"/json/list", &all); err != nil {
		return nil, err
	}
	pages := make([]Tab, 0, len(all))
	for _, t := range all {
		if t.Type == "page" {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

// ErrNotRunning is returned when something needs the browser and it is not up.
var ErrNotRunning = errors.New("the browser is not running")

// Open puts a URL in front of the person, starting the browser if needed.
//
// An existing tab on the same origin is reused and raised rather than adding a
// third Gmail tab to a window that already has two — the point is to show
// somebody a page, not to accumulate them.
func (s *Session) Open(ctx context.Context, url string) (Tab, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return Tab{}, errors.New("no URL to open")
	}
	if err := s.Start(ctx); err != nil {
		return Tab{}, err
	}
	endpoint := s.Endpoint(ctx)

	if tabs, err := s.Tabs(ctx); err == nil {
		for _, t := range tabs {
			if sameOrigin(t.URL, url) {
				if err := s.activate(ctx, endpoint, t.ID); err == nil {
					return t, nil
				}
			}
		}
	}

	var tab Tab
	if err := putJSON(ctx, endpoint+"/json/new?"+url, &tab); err != nil {
		return Tab{}, err
	}
	_ = s.activate(ctx, endpoint, tab.ID)
	s.closeBlanks(ctx, endpoint, tab.ID)
	return tab, nil
}

// closeBlanks tidies away the placeholder the window started with.
//
// Chromium needs something to open, so the launch passes about:blank; leaving
// it behind means every browser somebody is asked to sign into has an empty
// tab sitting next to the one that matters.
func (s *Session) closeBlanks(ctx context.Context, endpoint, keep string) {
	tabs, err := s.Tabs(ctx)
	if err != nil || len(tabs) < 2 {
		return
	}
	for _, t := range tabs {
		if t.ID == keep || t.URL != "about:blank" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json/close/"+t.ID, nil)
		if err != nil {
			continue
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
}

// Stop closes the browser. Sessions signed into it survive in the profile.
func (s *Session) Stop(ctx context.Context) error {
	endpoint := s.Endpoint(ctx)
	s.mu.Lock()
	cmd := s.cmd
	s.cmd = nil
	s.mu.Unlock()

	if endpoint != "" {
		// Ask first. A killed Chromium leaves the profile marked as crashed and
		// greets the person with a restore bar the next time they open it.
		ctxQuit, cancel := context.WithTimeout(ctx, 3*time.Second)
		req, _ := http.NewRequestWithContext(ctxQuit, http.MethodGet, endpoint+"/json/close", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
		cancel()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
	} else if st, ok := s.loadState(); ok && st.PID > 0 {
		if p, err := os.FindProcess(st.PID); err == nil {
			_ = p.Signal(os.Interrupt)
		}
	}
	s.clearState()
	return nil
}

// Profile is where the browser keeps its data, for a caller that wants to say
// so out loud.
func (s *Session) Profile() string { return s.dir }

func (s *Session) activate(ctx context.Context, endpoint, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json/activate/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("activate tab: %s", resp.Status)
	}
	return nil
}

func (s *Session) knownPort() int {
	s.mu.Lock()
	port := s.port
	s.mu.Unlock()
	if port != 0 {
		return port
	}
	if st, ok := s.loadState(); ok {
		s.mu.Lock()
		s.port = st.Port
		s.mu.Unlock()
		return st.Port
	}
	return 0
}

func (s *Session) statePath() string { return filepath.Join(s.dir, "session.json") }

func (s *Session) saveState(st state) {
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = os.WriteFile(s.statePath(), b, 0o600)
}

func (s *Session) loadState() (state, bool) {
	b, err := os.ReadFile(s.statePath())
	if err != nil {
		return state{}, false
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return state{}, false
	}
	return st, st.Port > 0
}

func (s *Session) clearState() {
	s.mu.Lock()
	s.port = 0
	s.mu.Unlock()
	_ = os.Remove(s.statePath())
}

// alive reports whether a DevTools endpoint is answering on this port.
func alive(ctx context.Context, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	var v struct {
		Browser string `json:"Browser"`
	}
	return getJSON(ctx, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), &v) == nil && v.Browser != ""
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("no free port for the browser: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func getJSON(ctx context.Context, url string, out any) error {
	return doJSON(ctx, http.MethodGet, url, out)
}

func putJSON(ctx context.Context, url string, out any) error {
	return doJSON(ctx, http.MethodPut, url, out)
}

func doJSON(ctx context.Context, method, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s", method, url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// sameOrigin compares scheme and host, so a second click on "connect Google"
// raises the tab already sitting on the consent screen.
func sameOrigin(a, b string) bool {
	oa, ok := origin(a)
	if !ok {
		return false
	}
	ob, ok := origin(b)
	return ok && oa == ob
}

func origin(raw string) (string, bool) {
	i := strings.Index(raw, "://")
	if i < 0 {
		return "", false
	}
	rest := raw[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		return "", false
	}
	return strings.ToLower(raw[:i] + "://" + rest), true
}

// Shared returns the one session for a data directory.
//
// The tool, the HTTP API and the coding harness all mean the same window when
// they say "the browser", and a second Session object pointed at the same
// profile would be a second Chromium refusing to start on a locked profile.
func Shared(dataDir string) *Session {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if s, ok := shared[dataDir]; ok {
		return s
	}
	s := New(dataDir)
	shared[dataDir] = s
	return s
}

var (
	sharedMu sync.Mutex
	shared   = map[string]*Session{}
)
