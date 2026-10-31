package desk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// isClaude is a command that runs Claude Code: "claude", "claude --model
// opus".
func isClaude(cmd string) bool {
	return cmd == "claude" || strings.HasPrefix(cmd, "claude ")
}

// claude's flags that choose its session, and whether each takes a value:
// -r's is optional.
var sessionFlags = map[string]bool{"--resume": true, "-r": true, "--session-id": true, "--continue": false, "-c": false, "--fork-session": false}

// choosesSession is a claude command that picks its own session: the desk
// does not give it one, and follows the one it runs.
func choosesSession(cmd string) bool {
	for _, a := range strings.Fields(cmd) {
		f, _, _ := strings.Cut(a, "=")
		if _, ok := sessionFlags[f]; ok {
			return true
		}
	}
	return false
}

// claudeLine is the shell line that runs Claude Code as cmd does. With an
// id, whatever chose the session at the launch goes, and the id is resumed
// (resume) or started (a session never written to cannot be resumed).
func claudeLine(bin, cmd, id string, resume bool) string {
	rest := strings.TrimSpace(strings.TrimPrefix(cmd, "claude"))
	line := shq(bin)
	if id != "" && sessionID.MatchString(id) { // it goes into the line as it is
		if choosesSession(rest) {
			rest = dropSessionFlags(rest)
		}
		if resume {
			line += " --resume " + id
		} else {
			line += " --session-id " + id
		}
	}
	if rest != "" {
		line += " " + rest
	}
	return line
}

func dropSessionFlags(args string) string {
	f := strings.Fields(args)
	var out []string
	for i := 0; i < len(f); i++ {
		name, _, eq := strings.Cut(f[i], "=")
		takes, ok := sessionFlags[name]
		if !ok {
			out = append(out, f[i])
			continue
		}
		if takes && !eq && i+1 < len(f) && !strings.HasPrefix(f[i+1], "-") {
			i++ // its value
		}
	}
	return strings.Join(out, " ")
}

var sessionID = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

// resumable is a Claude Code session with a transcript to resume.
func (m *Manager) resumable(id string) bool {
	if !sessionID.MatchString(id) {
		return false
	}
	hit, _ := filepath.Glob(filepath.Join(m.ClaudeDir, "projects", "*", id+".jsonl"))
	return len(hit) > 0
}

// claudeSession is what a claude process's session file says.
type claudeSession struct {
	PID       int    `json:"pid"`
	ID        string `json:"sessionId"`
	Status    string `json:"status"` // idle, busy, waiting
	ProcStart string `json:"procStart"`
}

// claudeUnder is the session of the first claude in pid's tree: the
// process whose session file (ClaudeDir/sessions/<pid>.json) is its own.
// A file is its process's when its start time is the process's: a pid is
// used again.
func (m *Manager) claudeUnder(pid int) (claudeSession, bool) {
	if pid <= 0 {
		return claudeSession{}, false
	}
	queue, seen := []int{pid}, 0
	for len(queue) > 0 && seen < 64 {
		p := queue[0]
		queue = queue[1:]
		seen++
		if s, ok := m.sessionOf(p); ok {
			return s, true
		}
		queue = append(queue, children(p)...)
	}
	return claudeSession{}, false
}

func (m *Manager) sessionOf(pid int) (claudeSession, bool) {
	b, err := os.ReadFile(filepath.Join(m.ClaudeDir, "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return claudeSession{}, false
	}
	var s claudeSession
	if json.Unmarshal(b, &s) != nil || !sessionID.MatchString(s.ID) || (s.PID != 0 && s.PID != pid) {
		return claudeSession{}, false
	}
	if s.ProcStart != "" {
		if st, ok := procStart(pid); !ok || st != s.ProcStart {
			return claudeSession{}, false
		}
	}
	return s, true
}

// children is pid's child processes, from each of its threads.
func children(pid int) []int {
	tasks, _ := filepath.Glob("/proc/" + strconv.Itoa(pid) + "/task/*/children")
	var out []int
	for _, t := range tasks {
		b, _ := os.ReadFile(t)
		for _, f := range strings.Fields(string(b)) {
			if n, err := strconv.Atoi(f); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// procStart is a process's start time, in clock ticks since boot (stat's
// 22nd field), as Claude Code writes it.
func procStart(pid int) (string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // the name may hold spaces and parens
	if i < 0 {
		return "", false
	}
	f := strings.Fields(s[i+1:]) // state is the 3rd field: f[0]
	if len(f) < 20 {
		return "", false
	}
	return f[19], true
}
