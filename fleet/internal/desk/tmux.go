package desk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// tmuxTimeout bounds one tmux call.
const tmuxTimeout = 15 * time.Second

// tmux runs a tmux command on the desk's server, in the sessions'
// environment.
func (m *Manager) tmux(ctx context.Context, args ...string) ([]byte, error) {
	return m.run(ctx, append([]string{m.Tmux}, m.tmuxArgs(args...)...))
}

func (m *Manager) tmuxArgs(args ...string) []string {
	out := []string{"-L", m.Socket}
	if m.Conf != "" {
		out = append(out, "-f", m.Conf)
	}
	return append(out, args...)
}

func (m *Manager) run(ctx context.Context, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, tmuxTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = m.env()
	cmd.Dir = m.Home
	cmd.WaitDelay = time.Second // a server that kept a pipe must not hold the call
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), errors.New(msg)
	}
	return out.Bytes(), nil
}

// gone is tmux saying the server or the session is not there. A server that
// is there but did not answer (busy, refused by permissions) is not gone:
// taken for gone, every session would be forgotten.
func gone(err error) bool {
	m := err.Error()
	if strings.Contains(m, "error connecting to") {
		return strings.Contains(m, "(No such file or directory)") || strings.Contains(m, "(Connection refused)")
	}
	return strings.Contains(m, "no server running") || strings.Contains(m, "can't find session")
}

// start makes a session, detached, in dir, running argv. When no server
// runs, it is started in a systemd scope of its own; sessions made later
// are the server's children, in its scope.
func (m *Manager) start(ctx context.Context, name, dir string, argv []string) error {
	args := append([]string{"new-session", "-d", "-s", name, "-c", dir, "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows), "--"}, argv...)
	if m.SystemdRun != "" && !m.serverUp(ctx) {
		unit := m.Socket + "-" + randHex(3)
		run := []string{m.SystemdRun, "--user", "--scope", "--collect", "--quiet", "--unit=" + unit,
			"--description=fleet desk (tmux -L " + m.Socket + ")", m.Tmux}
		_, err := m.run(ctx, append(run, m.tmuxArgs(args...)...))
		if err == nil {
			return nil
		}
		if _, herr := m.tmux(ctx, "has-session", "-t", "="+name); herr == nil {
			return nil
		}
		m.logf("systemd-run: %v; the server starts in fleetd's cgroup, and a restart of fleetd will end it", err)
	}
	if _, err := m.tmux(ctx, args...); err != nil {
		return fmt.Errorf("tmux new-session: %w", err)
	}
	return nil
}

func (m *Manager) serverUp(ctx context.Context) bool {
	_, err := m.tmux(ctx, "list-sessions", "-F", "#{session_name}")
	return err == nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// pane is a running session as tmux says: its clients, its pane's process
// and width; busy is Claude Code's, from its session file.
type pane struct {
	clients int
	pid     int
	cols    int
	busy    bool
}

// panes is every session on the server, in one call; none when no server
// runs.
func (m *Manager) panes(ctx context.Context) (map[string]pane, error) {
	out, err := m.tmux(ctx, "list-sessions", "-F", "#{session_name}\t#{session_attached}\t#{pane_pid}\t#{pane_width}")
	if err != nil {
		if gone(err) {
			return map[string]pane{}, nil
		}
		return nil, fmt.Errorf("tmux list-sessions: %w", err)
	}
	live := map[string]pane{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(l, "\t")
		if len(f) != 4 || f[0] == "" {
			continue
		}
		var p pane
		p.clients, _ = strconv.Atoi(f[1])
		p.pid, _ = strconv.Atoi(f[2])
		p.cols, _ = strconv.Atoi(f[3])
		live[f[0]] = p
	}
	return live, nil
}

// capture is a session's last lines, ANSI kept, after its blank padding.
func (m *Manager) capture(ctx context.Context, name string, lines int) (string, error) {
	out, err := m.tmux(ctx, "capture-pane", "-p", "-e", "-t", "="+name+":", "-S", "-"+strconv.Itoa(lines))
	if err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	return lastLines(string(out), lines), nil
}

// lastLines is the last n lines of a capture, after its blank padding.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// paneArgv is what a session's pane runs: its command in the operator's
// login shell, and the login shell after it, so the session stays when the
// command ends. Claude Code starts with the session id the desk chose, or,
// after a restart, resumes the session it ran last.
func (m *Manager) paneArgv(r record, restart bool) []string {
	if r.Command == "shell" {
		return []string{m.Shell, "-l"}
	}
	line := r.Command
	if isClaude(r.Command) {
		line = claudeLine(m.Claude, r.Command, r.Claude, restart && m.resumable(r.Claude))
	}
	return []string{m.Shell, "-lc", line + "; exec " + shq(m.Shell) + " -l"}
}

// shq quotes s for a POSIX shell.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// env is the sessions' environment.
func (m *Manager) env() []string {
	if m.Env != nil {
		return m.Env
	}
	return sessionEnv(os.Environ())
}

// systemdOwn is what systemd gives fleetd's service alone.
var systemdOwn = map[string]bool{
	"INVOCATION_ID": true, "JOURNAL_STREAM": true, "MANAGERPID": true, "MANAGERPIDFDID": true, "SYSTEMD_EXEC_PID": true,
	"NOTIFY_SOCKET": true, "WATCHDOG_PID": true, "WATCHDOG_USEC": true, "LISTEN_PID": true, "LISTEN_FDS": true,
	"LISTEN_FDNAMES": true, "MEMORY_PRESSURE_WATCH": true, "MEMORY_PRESSURE_WRITE": true, "TMUX": true, "TMUX_PANE": true,
}

// sessionEnv is fleetd's environment without what is fleetd's own: its
// FLEET_ settings, systemd's for its service, a tmux it runs in, and
// anything named like a secret. The login shell adds the operator's.
func sessionEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		if strings.HasPrefix(k, "FLEET_") || systemdOwn[k] || strings.Contains(u, "TOKEN") || strings.Contains(u, "SECRET") ||
			strings.Contains(u, "PASSWORD") || strings.Contains(u, "API_KEY") || strings.Contains(u, "CREDENTIAL") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
