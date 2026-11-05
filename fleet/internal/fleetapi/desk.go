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
	// GET ?lines=N: the sessions, with their screens; with since=V and
	// wait=S (seconds, at most DeskMaxWait) it answers once the list is no
	// longer version V, or after S seconds with the list as it is. POST
	// DeskCreate.
	PathDesk     = "/v1/desk"
	PathDeskDirs = "/v1/desk/dirs" // GET ?prefix=P: directories on fleetd's machine, for the launch form
	// DELETE PathDesk + "/{name}" ends a session and forgets it; PATCH
	// PathDesk + "/{name}" with DeskUpdate changes its label or workspace, or
	// moves it.

	// PathDeskWorkspaces: POST DeskWorkspaceCreate makes one; PATCH
	// PathDeskWorkspaces + "/{id}" with DeskWorkspaceUpdate renames it,
	// changes its layout or moves it; DELETE PathDeskWorkspaces + "/{id}"
	// removes an empty one (?end=1 ends its sessions first).
	PathDeskWorkspaces = "/v1/desk/workspaces"
	// PathDeskPaste: POST ?session=S&file=F, the file's bytes as the body,
	// writes it on fleetd's machine for session S and answers DeskPasted:
	// the path to type into the session. With kind=tar the body is a tar
	// (gzipped or not) of a folder named F, unpacked there.
	PathDeskPaste = "/v1/desk/paste"
	// PathDeskFile: GET ?path=P is the regular file at P on fleetd's
	// machine, as a download.
	PathDeskFile = "/v1/desk/file"
)

// DeskMaxWait is the longest a GET PathDesk waits for a change, in seconds.
const DeskMaxWait = 50

// DeskMaxPaste is the largest body PathDeskPaste takes.
const DeskMaxPaste = 512 << 20

// Workspace layouts: how a workspace's sessions share the screen.
const (
	LayoutTabs    = "tabs"    // one at a time, a tab each
	LayoutColumns = "columns" // side by side
	LayoutRows    = "rows"    // one above another
	LayoutGrid    = "grid"    // tiled
	LayoutMain    = "main"    // the first large on the left, the rest stacked on the right
)

// Layouts is every layout, the first the default.
var Layouts = []string{LayoutTabs, LayoutColumns, LayoutRows, LayoutGrid, LayoutMain}

// DefaultWorkspace is the workspace a session without one belongs to.
const DefaultWorkspace = "main"

// Claude Code's state in a session, from its own session file.
const (
	StatusWorking = "working" // in a turn
	StatusWaiting = "waiting" // asks the operator something (a permission, a question)
	StatusIdle    = "idle"    // its turn is over: the operator's move
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
	// Workspace is the id of the workspace it is in; Label the name the
	// operator gave it ("" shows Name).
	Workspace string `json:"workspace"`
	Label     string `json:"label,omitempty"`
	// Title is what the program in it calls it (its terminal title, Claude
	// Code's summary of the conversation), without a spinner.
	Title string `json:"title,omitempty"`
	// Status is Claude Code's (Status*); "" when no Claude Code runs in it.
	Status string `json:"status,omitempty"`
	// Cwd is the directory its pane is in now, and Branch that directory's
	// git branch.
	Cwd    string `json:"cwd,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Note is what its Claude Code last said it needs (its hooks), nil when
	// nothing: the operator answered, or no hook has run.
	Note *DeskNote `json:"note,omitempty"`
	// Its screen's last lines (ANSI kept) and width, when asked for lines.
	Text  string    `json:"text,omitempty"`
	Cols  int       `json:"cols,omitempty"`
	At    time.Time `json:"at,omitzero"` // when the screen was captured
	Error string    `json:"error,omitempty"`
}

// DeskNote is what a session's Claude Code said through its hooks.
type DeskNote struct {
	Kind string    `json:"kind"` // Note*
	Text string    `json:"text"` // Claude Code's words: "needs your permission to use Bash"
	At   time.Time `json:"at"`
}

// Note kinds.
const (
	NoteNeeds = "needs" // it asks the operator something, or waits for them
	NoteDone  = "done"  // its turn ended
)

// DeskList is GET /v1/desk.
type DeskList struct {
	Host string `json:"host"` // the machine they run on
	// Version changes whenever anything listed does (screens aside): what
	// a waiting GET is given as since.
	Version    string          `json:"version,omitempty"`
	Workspaces []DeskWorkspace `json:"workspaces"` // in their order
	Sessions   []DeskSession   `json:"sessions"`   // in their order, every workspace's
}

// DeskWorkspace is a named group of sessions, shown together in its layout.
type DeskWorkspace struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Layout  string    `json:"layout"` // Layout*
	Order   int       `json:"order"`
	Created time.Time `json:"created"`
}

// DeskCreate is POST /v1/desk's body. Name "" picks one from the dir;
// Command "" is claude; "shell" is a login shell. Workspace "" is the
// first workspace; Label "" shows the name. Branch, when set, starts the
// session in a worktree of Dir's repository on that branch instead, made
// by worktrunk (wt) if there is none: the branch too, when it is new.
type DeskCreate struct {
	Name      string `json:"name"`
	Dir       string `json:"dir"`
	Command   string `json:"command"`
	Workspace string `json:"workspace,omitempty"`
	Label     string `json:"label,omitempty"`
	Branch    string `json:"branch,omitempty"`
}

// DeskWorktreeWait is how long fleetd waits on worktrunk for a Branch's
// worktree (it runs the repository's pre-start hooks first): a client
// waits that much longer for the answer.
const DeskWorktreeWait = 3 * time.Minute

// DeskUpdate is PATCH /v1/desk/{name}'s body; a nil field stays. Before
// moves the session in front of the named one (in Workspace, or its own);
// "" moves it to its workspace's end.
type DeskUpdate struct {
	Label     *string `json:"label,omitempty"`
	Workspace *string `json:"workspace,omitempty"`
	Before    *string `json:"before,omitempty"`
}

// DeskWorkspaceCreate is POST /v1/desk/workspaces's body; Layout "" is
// tabs.
type DeskWorkspaceCreate struct {
	Name   string `json:"name"`
	Layout string `json:"layout,omitempty"`
}

// DeskWorkspaceUpdate is PATCH /v1/desk/workspaces/{id}'s body; a nil field
// stays. Before moves the workspace in front of the one with that id; ""
// moves it to the end.
type DeskWorkspaceUpdate struct {
	Name   *string `json:"name,omitempty"`
	Layout *string `json:"layout,omitempty"`
	Before *string `json:"before,omitempty"`
}

// DeskPasted is what POST /v1/desk/paste wrote: the path to type.
type DeskPasted struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}
