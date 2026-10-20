// Package target reaches a container through its host's exec prefix —
// [docker] locally, [docker, --context, pc2] for another machine. Nothing in
// the fleet talks to a container any other way, which is what keeps it
// generic: a podman or ssh prefix is a config change.
package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Runner runs one host command. The real one is exec; tests fake it.
type Runner interface {
	Run(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error)
}

// Exec runs commands on this machine.
type Exec struct{}

// Run runs argv and returns its stdout; a failure carries stderr.
func (Exec) Run(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	cmd := HardenCtx(exec.CommandContext(ctx, argv[0], argv[1:]...))
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		if ce := ctx.Err(); ce != nil && !errors.Is(err, ce) {
			err = fmt.Errorf("%w: %w", ce, err) // killed because the caller gave up
		}
		return out.Bytes(), fmt.Errorf("%s: %w: %s", strings.Join(argv[:min(len(argv), 6)], " "), err, msg)
	}
	return out.Bytes(), nil
}

// Container is one container on one host. The zero prefix and name is this
// machine itself, outside any container: an orchestrator running natively on
// fleetd's host.
type Container struct {
	Exec   []string
	Name   string
	Runner Runner
}

func (c Container) runner() Runner {
	if c.Runner == nil {
		return Exec{}
	}
	return c.Runner
}

func (c Container) native() bool { return len(c.Exec) == 0 && c.Name == "" }

func (c Container) argv(flags []string, cmd []string) []string {
	if c.native() {
		// Only -e reaches a native command: stdin is always passed, and
		// there is no terminal flag to give.
		var env []string
		for i := 0; i+1 < len(flags); i++ {
			if flags[i] == "-e" {
				env = append(env, flags[i+1])
				i++
			}
		}
		if len(env) == 0 {
			return append([]string{}, cmd...)
		}
		return append(append([]string{"env"}, env...), cmd...)
	}
	out := append([]string{}, c.Exec...)
	out = append(out, "exec")
	out = append(out, flags...)
	out = append(out, c.Name)
	return append(out, cmd...)
}

// Run runs a command inside the container.
func (c Container) Run(ctx context.Context, cmd ...string) ([]byte, error) {
	return c.runner().Run(ctx, c.argv(nil, cmd), nil)
}

// RunIn runs a command inside the container with stdin.
func (c Container) RunIn(ctx context.Context, stdin io.Reader, cmd ...string) ([]byte, error) {
	return c.runner().Run(ctx, c.argv([]string{"-i"}, cmd), stdin)
}

// RunInEnv runs a command inside the container with stdin and extra env.
func (c Container) RunInEnv(ctx context.Context, env []string, stdin io.Reader, cmd ...string) ([]byte, error) {
	flags := []string{"-i"}
	for _, e := range env {
		flags = append(flags, "-e", e)
	}
	return c.runner().Run(ctx, c.argv(flags, cmd), stdin)
}

// Sh runs a shell script inside the container, with args as $1….
func (c Container) Sh(ctx context.Context, script string, args ...string) ([]byte, error) {
	return c.Run(ctx, append([]string{"sh", "-c", script, "sh"}, args...)...)
}

// InteractiveArgv is the argv for a terminal session inside the container,
// for the caller to run attached to its own terminal.
func (c Container) InteractiveArgv(cmd ...string) []string {
	return c.argv([]string{"-it"}, cmd)
}

// Running reports whether the container is up.
func (c Container) Running(ctx context.Context) (bool, error) {
	s, err := c.State(ctx)
	return s.Running, err
}

// State is a container as Docker reports it.
type State struct {
	Running bool
	// Failed is Docker's error when the container is stopped because Docker
	// could not start it — a bind mount whose source was not there yet, after
	// a power cut — which its restart policy never retries. It is empty when
	// the container runs, or was stopped on purpose.
	Failed string
}

// State reads the container's state.
func (c Container) State(ctx context.Context) (State, error) {
	if c.native() {
		return State{Running: true}, nil
	}
	argv := append(append([]string{}, c.Exec...), "inspect", "-f", "{{.State.Running}}\t{{.State.Status}}\t{{.State.Error}}", c.Name)
	out, err := c.runner().Run(ctx, argv, nil)
	if err != nil {
		if missing(err) {
			return State{}, nil // removed (fleetctl down): stopped, with nothing to start
		}
		// The host did not answer: that says nothing about the container.
		return State{}, err
	}
	f := strings.SplitN(strings.TrimSpace(string(out)), "\t", 3)
	s := State{Running: f[0] == "true"}
	if !s.Running && len(f) == 3 && (f[1] == "exited" || f[1] == "created") {
		s.Failed = strings.TrimSpace(f[2])
	}
	return s, nil
}

// missing is Docker saying the container does not exist.
func missing(err error) bool {
	m := err.Error()
	return strings.Contains(m, "No such object") || strings.Contains(m, "No such container")
}

// Start starts the stopped container.
func (c Container) Start(ctx context.Context) error {
	_, err := c.runner().Run(ctx, append(append([]string{}, c.Exec...), "start", c.Name), nil)
	return err
}

// Host runs a docker subcommand against the host itself (stats, compose…).
func (c Container) Host(ctx context.Context, args ...string) ([]byte, error) {
	return c.runner().Run(ctx, append(append([]string{}, c.Exec...), args...), nil)
}
