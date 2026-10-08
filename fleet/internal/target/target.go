// Package target reaches a container through its host's exec prefix —
// [docker] locally, [docker, --context, pc2] for another machine. Nothing in
// the fleet talks to a container any other way, which is what keeps it
// generic: a podman or ssh prefix is a config change.
package target

import (
	"bytes"
	"context"
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
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		return out.Bytes(), fmt.Errorf("%s: %w: %s", strings.Join(argv[:min(len(argv), 6)], " "), err, msg)
	}
	return out.Bytes(), nil
}

// Container is one container on one host.
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

func (c Container) argv(flags []string, cmd []string) []string {
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
	argv := append(append([]string{}, c.Exec...), "inspect", "-f", "{{.State.Running}}", c.Name)
	out, err := c.runner().Run(ctx, argv, nil)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// Host runs a docker subcommand against the host itself (stats, compose…).
func (c Container) Host(ctx context.Context, args ...string) ([]byte, error) {
	return c.runner().Run(ctx, append(append([]string{}, c.Exec...), args...), nil)
}
