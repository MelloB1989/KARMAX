// Package sandbox runs a coding agent somewhere it cannot hurt anything.
//
// A sandbox is a container with Claude Code, git and a scoped token, handed a
// ticket and left to produce a branch. The daemon does not wait on it and does
// not believe it: the container's own report is a log line, and the case
// advances when the pull-request webhook arrives. That split is deliberate —
// a container that dies mid-run, or lies, must not be able to move the work
// forward on its say-so.
package sandbox

import (
	"context"
	"time"
)

// Container states. Everything a driver reports maps onto one of these.
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateExited   = "exited"
	StateFailed   = "failed"
	// StateGone is a container the driver no longer knows about — killed by
	// hand, reaped by the host, or lost with the machine. Distinct from failed
	// because there is no exit code to report and nothing to collect.
	StateGone = "gone"
)

// Spec is one unit of sandboxed work.
type Spec struct {
	Image  string
	Repo   string
	Branch string
	// Task is what the agent inside is asked to do, in prose.
	Task string
	// Env is injected as-is. The caller supplies credentials here — the token
	// for the coding agent and a repo-scoped git token — so the driver never
	// reads the credential store itself.
	Env     map[string]string
	Timeout time.Duration
}

// Status is where a container has got to.
type Status struct {
	ID       string
	State    string
	ExitCode int
	LogTail  string
}

// Driver is one way of running a container: a local Docker socket, an ECS task,
// a Kubernetes job. The interface is deliberately small — launch it, ask about
// it, read it, stop it — because everything durable lives in sandbox_runs
// rather than in the driver.
type Driver interface {
	Name() string
	Launch(ctx context.Context, s Spec) (id string, err error)
	Poll(ctx context.Context, id string) (Status, error)
	Logs(ctx context.Context, id string, tail int) (string, error)
	Kill(ctx context.Context, id string) error
}
