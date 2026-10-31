package fleetapi

import (
	"strings"
	"time"
)

// The desk: tmux sessions on fleetd's own machine, for the operator alone
// (admins). Each runs a command in a directory, Claude Code by default, and
// is brought back after the machine restarts, in its place in the order, its
// Claude Code session resumed.
const (
	PathDesk     = "/v1/desk"      // GET ?lines=N: the sessions, with their screens; POST DeskCreate
	PathDeskDirs = "/v1/desk/dirs" // GET ?prefix=P: directories on fleetd's machine, for the launch form
	// DELETE PathDesk + "/{name}" ends a session and forgets it.
)

// DeskPrefix names a desk session where a worker is expected: the terminal
// route (fleetctl ssh, the TUI's attach) takes desk:<name> for one.
const DeskPrefix = "desk:"

// DeskWorker is the terminal route's name for desk session name.
func DeskWorker(name string) string { return DeskPrefix + name }

// DeskName is the session a terminal name is for, if it is one.
func DeskName(worker string) (string, bool) { return strings.CutPrefix(worker, DeskPrefix) }

// DeskSession is one session on the desk.
type DeskSession struct {
	Name    string    `json:"name"`
	Dir     string    `json:"dir"`
	Command string    `json:"command"` // as launched: "claude", "claude --model opus", "shell"
	Order   int       `json:"order"`   // its place: sessions come back in this order
	Created time.Time `json:"created"`
	Running bool      `json:"running"` // its tmux session is up
	// Claude is the Claude Code session it runs (the latest, after a /clear
	// or a resume): the one a restart resumes.
	Claude   string    `json:"claude_session,omitempty"`
	Restored time.Time `json:"restored,omitzero"` // when a restart last brought it back
	Busy     bool      `json:"busy,omitempty"`    // Claude Code is in a turn
	Clients  int       `json:"clients,omitempty"` // terminals attached now
	// Its screen's last lines (ANSI kept) and width, when asked for lines.
	Text  string    `json:"text,omitempty"`
	Cols  int       `json:"cols,omitempty"`
	At    time.Time `json:"at,omitzero"` // when the screen was captured
	Error string    `json:"error,omitempty"`
}

// DeskList is GET /v1/desk.
type DeskList struct {
	Host     string        `json:"host"` // the machine they run on
	Sessions []DeskSession `json:"sessions"`
}

// DeskCreate is POST /v1/desk's body. Name "" picks one from the dir;
// Command "" is claude; "shell" is a login shell.
type DeskCreate struct {
	Name    string `json:"name"`
	Dir     string `json:"dir"`
	Command string `json:"command"`
}
