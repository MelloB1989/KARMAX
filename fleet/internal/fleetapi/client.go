package fleetapi

import (
	"context"
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
