package fleetapi

import (
	"context"
	"io"
	"time"
)

// Client is everything the TUI asks of fleetd. fleetctl implements it over
// HTTP with the routes its commands already use; Fake implements it for
// tests and `fleetctl tui --demo`. The TUI has no other way to fleetd.
type Client interface {
	// Watch returns the first snapshot newer than since, waiting up to the
	// server's long-poll limit; since 0 returns the current one at once.
	Watch(ctx context.Context, since uint64) (*Snapshot, error)
	Screens(ctx context.Context, workers []string, lines int) ([]Screen, error)
	Task(ctx context.Context, id string) (*TaskDetail, error)
	Run(ctx context.Context, r RunRequest) (*RunReply, error)
	Followup(ctx context.Context, task, text string) error
	Answer(ctx context.Context, task, text string) error
	Cancel(ctx context.Context, task, reason string) error
	Done(ctx context.Context, task string) error
	SetModel(ctx context.Context, worker, model string) error // admins
	Access(ctx context.Context) (*AccessList, error)          // admins
	ChangeAccess(ctx context.Context, c AccessChange) (*AccessList, error)
	AWS(ctx context.Context, refresh bool) (*AWSReport, error) // admins
	Desk(ctx context.Context, lines int) (*DeskList, error)    // admins
	DeskCreate(ctx context.Context, c DeskCreate) (*DeskSession, error)
	DeskEnd(ctx context.Context, name string) error
	DeskDirs(ctx context.Context, prefix string) ([]string, error)
	// DeskWatch is the desk once it is no longer version since (or after
	// fleetd's wait, as it is); since "" answers at once.
	DeskWatch(ctx context.Context, since string) (*DeskList, error)
	DeskUpdate(ctx context.Context, name string, u DeskUpdate) error
	DeskWorkspaceCreate(ctx context.Context, c DeskWorkspaceCreate) (*DeskWorkspace, error)
	DeskWorkspaceUpdate(ctx context.Context, id string, u DeskWorkspaceUpdate) error
	DeskWorkspaceDelete(ctx context.Context, id string, end bool) error
	// DeskPaste sends a file (tar: a folder's tar) pasted into session to
	// fleetd's machine; size is -1 when unknown.
	DeskPaste(ctx context.Context, session, file string, body io.Reader, size int64, tar bool) (*DeskPasted, error)
	// DeskTerm types in a desk session: its terminal, cols×rows, until
	// Close. force takes the keyboard from whoever has it.
	DeskTerm(ctx context.Context, name string, cols, rows int, force bool) (TermConn, error)
	// Files is fleetd's machine's files (files.go); admins.
	Files
}

// TermConn is a terminal session on fleetd (GET /v1/term/{worker}).
type TermConn interface {
	// Recv is the next thing fleetd sends: the terminal's bytes, or a
	// control frame (hello, held, released, error, exit).
	Recv(ctx context.Context) (data []byte, frame *TermFrame, err error)
	// Send types data.
	Send(ctx context.Context, data []byte) error
	// Resize says the terminal is now cols×rows.
	Resize(ctx context.Context, cols, rows int) error
	Close() error
}

// TaskDetail is GET /v1/tasks/{id} (daemon.TaskResult), field for field.
type TaskDetail struct {
	Task         Task      `json:"task"`
	State        string    `json:"state"` // open | closed
	Reports      []Message `json:"reports"`
	Commits      []string  `json:"commits"`
	Ahead        int       `json:"ahead"`
	CommitsError string    `json:"commits_error,omitempty"`
}

// Message is one message of a task's thread (ledger.Relay).
type Message struct {
	ID        int64     `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Text      string    `json:"text"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
	Delivered bool      `json:"delivered"`
	Task      string    `json:"task,omitempty"`
	Kind      string    `json:"kind,omitempty"` // report | question | progress
	Held      bool      `json:"held,omitempty"`
}

// RunRequest is POST /v1/run's body, as the TUI's run form fills it.
type RunRequest struct {
	Brief string `json:"brief"`
	Repo  string `json:"repo,omitempty"`
	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
}

// RunReply is POST /v1/run's answer.
type RunReply struct {
	Task  string `json:"task"`
	Agent string `json:"agent"`
	Why   string `json:"why"`
}
