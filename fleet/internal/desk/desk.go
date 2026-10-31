// Package desk is the operator's tmux sessions on fleetd's own machine: each
// runs a command in a directory, Claude Code by default, on a tmux server of
// the desk's own (tmux -L fleet-desk), never the operator's default one.
//
// The server is started in a systemd scope of its own, outside fleetd's
// cgroup, so that restarting fleetd does not end it. After the machine
// restarts, every session is made again, in its order and directory, its
// Claude Code session resumed. The record is desk.json in fleetd's state
// directory, with the boot it was written in: a session missing on the same
// boot was closed by the operator, and is forgotten.
package desk

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/term"
)

// ErrNoSession is a name the desk has no session by.
var ErrNoSession = errors.New("no desk session")

// ErrInvalid is a request the desk refuses: a directory that is not one.
var ErrInvalid = errors.New("invalid desk request")

type invalid struct{ error }

func (invalid) Is(t error) bool { return t == ErrInvalid }

func invalidf(format string, a ...any) error { return invalid{fmt.Errorf(format, a...)} }

// Every new session's window, until a typist's terminal sizes it.
const cols, rows = 200, 50

// SyncEvery is how often Run looks at the sessions on its own: a server
// found gone, Claude Code's session followed.
var SyncEvery = 30 * time.Second

// Manager is the desk. New fills in its defaults; tests change them.
type Manager struct {
	Socket string // tmux -L
	State  string // desk.json
	Tmux   string // the tmux binary
	// Conf is the server's config (tmux -f); "" is the operator's own.
	Conf string
	// Shell is the login shell every command runs in, so that its PATH and
	// environment are the operator's.
	Shell string
	// Claude is Claude Code's command, found on the login shell's PATH.
	Claude string
	// ClaudeDir is Claude Code's own (~/.claude): sessions/ says which
	// session each claude process runs, projects/ which ones can resume.
	ClaudeDir string
	Home      string
	// SystemdRun, when set, starts the tmux server in a scope of its own
	// (systemd-run --user --scope), outside fleetd's cgroup: a restart of
	// fleetd ends everything in that. "" starts it plainly.
	SystemdRun string
	// BootID is this boot's id.
	BootID func() string
	// Env is the sessions' environment; nil is fleetd's, without what is
	// fleetd's own (sessionEnv).
	Env  []string
	Now  func() time.Time
	Log  func(format string, args ...any)
	Host string // the machine, as the list names it

	mu     sync.Mutex
	st     state
	loaded bool
}

// state is desk.json.
type state struct {
	Boot     string   `json:"boot_id"`
	Sessions []record `json:"sessions"` // in their order
}

// record is a session as the desk keeps it.
type record struct {
	Name     string    `json:"name"`
	Dir      string    `json:"dir"`
	Command  string    `json:"command"`
	Order    int       `json:"order"`
	Created  time.Time `json:"created"`
	Claude   string    `json:"claude_session,omitempty"`
	Restored time.Time `json:"restored,omitzero"`
	// Error is why a restart could not bring it back. It stays so, listed,
	// until it is ended.
	Error string `json:"error,omitempty"`
}

// New is the desk on tmux socket, its record in stateDir.
func New(socket, stateDir string) *Manager {
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	m := &Manager{Socket: socket, State: filepath.Join(stateDir, "desk.json"), Tmux: "tmux", Shell: loginShell(),
		Claude: "claude", ClaudeDir: filepath.Join(home, ".claude"), Home: home, BootID: bootID, Now: time.Now, Host: host}
	if p, err := exec.LookPath("systemd-run"); err == nil {
		m.SystemdRun = p
	}
	return m
}

func bootID() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

// loginShell is the operator's: $SHELL, or /etc/passwd's, or sh.
func loginShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	if b, err := os.ReadFile("/etc/passwd"); err == nil {
		uid := fmt.Sprint(os.Getuid())
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Split(l, ":"); len(f) >= 7 && f[2] == uid && f[6] != "" {
				return f[6]
			}
		}
	}
	return "/bin/sh"
}

func (m *Manager) logf(format string, args ...any) {
	if m.Log != nil {
		m.Log("desk: "+format, args...)
	}
}

// Run brings the desk back after the machine restarted, then looks at it
// every SyncEvery, until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	var last string
	t := time.NewTicker(SyncEvery)
	defer t.Stop()
	for {
		why := ""
		if err := m.Sync(ctx); err != nil {
			why = err.Error()
		}
		if why != last && why != "" { // once per new failure, not every tick
			m.logf("%s", why)
		}
		last = why
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sync brings the record and tmux in line (sync).
func (m *Manager) Sync(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.sync(ctx)
	return err
}

// sync brings the record and tmux in line, and returns what runs. On a new
// boot, every session not running is made again, in order. On the same
// boot, one not running was closed, and is forgotten. Each running session's
// Claude Code session is followed. m.mu is held.
func (m *Manager) sync(ctx context.Context) (map[string]pane, error) {
	m.load()
	live, err := m.panes(ctx)
	if err != nil {
		return nil, err
	}
	changed := false
	if boot := m.BootID(); m.st.Boot != boot {
		for i := range m.st.Sessions {
			r := &m.st.Sessions[i]
			if _, up := live[r.Name]; up {
				continue
			}
			if err := m.restore(ctx, r); err != nil {
				if ctx.Err() != nil { // fleetd is stopping: the rest come back at its next start
					m.save()
					return nil, ctx.Err()
				}
				r.Error = "not restored: " + err.Error()
				m.logf("%s: %s", r.Name, r.Error)
				continue
			}
			r.Restored, r.Error = m.Now(), ""
			m.logf("%s: restored in %s", r.Name, r.Dir)
		}
		m.st.Boot, changed = boot, true
		if live, err = m.panes(ctx); err != nil {
			m.save()
			return nil, err
		}
	} else {
		keep := m.st.Sessions[:0]
		for _, r := range m.st.Sessions {
			if _, up := live[r.Name]; up || r.Error != "" {
				keep = append(keep, r)
				continue
			}
			m.logf("%s: closed", r.Name)
			changed = true
		}
		m.st.Sessions = keep
		m.renumber()
	}
	for i := range m.st.Sessions {
		r := &m.st.Sessions[i]
		p, up := live[r.Name]
		if !up {
			continue
		}
		if c, ok := m.claudeUnder(p.pid); ok {
			if c.ID != r.Claude {
				r.Claude, changed = c.ID, true
			}
			p.busy = c.Status == "busy"
			live[r.Name] = p
		}
	}
	if changed {
		m.save()
	}
	return live, nil
}

// restore makes r's session again, after a restart: Claude Code resumes the
// session it ran last.
func (m *Manager) restore(ctx context.Context, r *record) error {
	if fi, err := os.Stat(r.Dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is gone", r.Dir)
	}
	return m.start(ctx, r.Name, r.Dir, m.paneArgv(*r, true))
}

func (m *Manager) renumber() {
	for i := range m.st.Sessions {
		m.st.Sessions[i].Order = i
	}
}

// Create starts a session and records it.
func (m *Manager) Create(ctx context.Context, c fleetapi.DeskCreate) (fleetapi.DeskSession, error) {
	dir, err := m.dir(c.Dir)
	if err != nil {
		return fleetapi.DeskSession{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	live, err := m.sync(ctx)
	if err != nil {
		return fleetapi.DeskSession{}, err
	}
	name := c.Name
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(dir)
	}
	name = m.unique(cleanName(name), live)
	cmd := strings.TrimSpace(c.Command)
	if cmd == "" {
		cmd = "claude"
	}
	r := record{Name: name, Dir: dir, Command: cmd, Order: len(m.st.Sessions), Created: m.Now()}
	if isClaude(cmd) && !choosesSession(cmd) {
		r.Claude = newUUID()
	}
	if err := m.start(ctx, r.Name, r.Dir, m.paneArgv(r, false)); err != nil {
		return fleetapi.DeskSession{}, err
	}
	m.st.Sessions = append(m.st.Sessions, r)
	m.save()
	m.logf("%s: started in %s: %s", r.Name, r.Dir, r.Command)
	s := r.view()
	s.Running = true
	return s, nil
}

// dir is a directory asked for: ~ expanded, relative to home, and there.
func (m *Manager) dir(d string) (string, error) {
	p := m.expand(strings.TrimSpace(d))
	fi, err := os.Stat(p)
	switch {
	case err != nil:
		return "", invalidf("dir %s: no such directory", p)
	case !fi.IsDir():
		return "", invalidf("dir %s: not a directory", p)
	}
	return p, nil
}

// cleanName is a tmux session name: letters, digits, - and _ (tmux makes
// a . or : into _ itself, and the record must say what tmux does).
func cleanName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		case r == '.' || r == ':':
			b.WriteRune('_')
		default:
			b.WriteRune('-')
		}
	}
	n := b.String()
	for strings.Contains(n, "--") {
		n = strings.ReplaceAll(n, "--", "-")
	}
	n = strings.Trim(n, "-_")
	if len(n) > 40 {
		n = strings.Trim(n[:40], "-_")
	}
	if n == "" {
		return "desk"
	}
	return n
}

// unique is name, or name-2, name-3…: one no session has, recorded or on
// the server.
func (m *Manager) unique(name string, live map[string]pane) string {
	taken := func(n string) bool {
		if _, ok := live[n]; ok {
			return true
		}
		for _, r := range m.st.Sessions {
			if r.Name == n {
				return true
			}
		}
		return false
	}
	n := name
	for i := 2; taken(n); i++ {
		n = fmt.Sprintf("%s-%d", name, i)
	}
	return n
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// End ends a session and forgets it; the rest close up the order.
func (m *Manager) End(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	i := m.find(name)
	if i < 0 {
		return fmt.Errorf("%w %q", ErrNoSession, name)
	}
	if _, err := m.tmux(ctx, "kill-session", "-t", "="+name); err != nil && !gone(err) {
		return fmt.Errorf("end %s: %w", name, err)
	}
	m.st.Sessions = append(m.st.Sessions[:i], m.st.Sessions[i+1:]...)
	m.renumber()
	m.save()
	m.logf("%s: ended", name)
	return nil
}

func (m *Manager) find(name string) int {
	for i, r := range m.st.Sessions {
		if r.Name == name {
			return i
		}
	}
	return -1
}

// maxLines bounds a screen.
const maxLines = 500

// List is every session, in order, with its screen's last lines when lines
// is above 0.
func (m *Manager) List(ctx context.Context, lines int) (fleetapi.DeskList, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := fleetapi.DeskList{Host: m.Host, Sessions: []fleetapi.DeskSession{}}
	live, err := m.sync(ctx)
	if err != nil { // tmux did not answer: what is recorded, and why no more
		m.load()
		for _, r := range m.st.Sessions {
			s := r.view()
			s.Error = err.Error()
			out.Sessions = append(out.Sessions, s)
		}
		return out, nil
	}
	lines = min(lines, maxLines)
	for _, r := range m.st.Sessions {
		s := r.view()
		if p, up := live[r.Name]; up {
			s.Running, s.Clients, s.Busy, s.Cols = true, p.clients, p.busy, p.cols
			if lines > 0 {
				text, err := m.capture(ctx, r.Name, lines)
				s.At = m.Now()
				if err != nil {
					s.Error = err.Error()
				} else {
					s.Text = text
				}
			}
		}
		out.Sessions = append(out.Sessions, s)
	}
	return out, nil
}

func (r record) view() fleetapi.DeskSession {
	return fleetapi.DeskSession{Name: r.Name, Dir: r.Dir, Command: r.Command, Order: r.Order, Created: r.Created,
		Claude: r.Claude, Restored: r.Restored, Error: r.Error}
}

// Terminal is where the terminal route reaches a session: its tmux session
// on this machine, and a login shell in its directory.
func (m *Manager) Terminal(name string) (term.Target, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	i := m.find(name)
	if i < 0 {
		return term.Target{}, false
	}
	r := m.st.Sessions[i]
	return term.Target{Worker: fleetapi.DeskWorker(r.Name), Host: m.Host, Pane: r.Name, Socket: m.Socket, Dir: r.Dir, Shell: m.Shell}, true
}

// load reads desk.json once. One that cannot be read is kept aside, and the
// desk starts empty.
func (m *Manager) load() {
	if m.loaded {
		return
	}
	m.loaded = true
	b, err := os.ReadFile(m.State)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.logf("read %s: %v", m.State, err)
		}
		return
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		m.logf("%s: %v; kept as %s.bad, starting empty", m.State, err, m.State)
		os.Rename(m.State, m.State+".bad")
		return
	}
	m.st = st
	m.renumber()
}

// save writes desk.json whole, 0600, by rename.
func (m *Manager) save() {
	if err := m.write(); err != nil {
		m.logf("save %s: %v", m.State, err)
	}
}

func (m *Manager) write() error {
	if m.st.Sessions == nil {
		m.st.Sessions = []record{}
	}
	b, err := json.MarshalIndent(m.st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.State)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".desk-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), m.State)
}
