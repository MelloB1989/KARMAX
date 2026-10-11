// Package hostctl is the argv and the reporting behind `fleetctl render`,
// `up` and `down`, shared by the CLI on a fleet host and by fleetd's routes
// (which run them for a remote operator).
package hostctl

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/outbound"
	"github.com/MelloB1989/karmax/fleet/internal/render"
)

// Runner runs one command, writing its output to w.
type Runner func(ctx context.Context, argv []string, w io.Writer) error

// ExecRunner runs the command for real.
func ExecRunner(ctx context.Context, argv []string, w io.Writer) error {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdout, c.Stderr = w, w
	return c.Run()
}

// RenderDir is where renders go unless told otherwise.
func RenderDir(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "render") }

// Render writes compose files and agent env files (out "" is RenderDir) and
// describes what it wrote.
func Render(cfg *config.Config, out string, w io.Writer) error {
	if out == "" {
		out = RenderDir(cfg)
	}
	env, err := outbound.ReadEnv(cfg.EnvFile)
	if err != nil {
		return fmt.Errorf("env file: %w", err)
	}
	if err := render.Render(cfg, env, out); err != nil {
		return err
	}
	fmt.Fprintf(w, "rendered into %s:\n", out)
	for _, h := range cfg.HostNames() {
		if !render.Runs(cfg, h) {
			fmt.Fprintf(w, "  %s: nothing to run, no compose file\n", h)
			continue
		}
		fmt.Fprintf(w, "  compose.%s.yaml  (%s)\n", h, strings.Join(cfg.AgentsOn(h), ", "))
	}
	fmt.Fprintln(w, "  agents/*.env      (0600, one token each)")
	if p := render.OrchestratorBriefPath(cfg); p != "" {
		fmt.Fprintf(w, "orchestrator brief: %s\n", p)
	} else {
		fmt.Fprintln(w, "orchestrator brief: not written — set orchestrator.karmax_agent in fleet.yaml")
	}
	return nil
}

// Argv is the command that brings one host's compose project up or down.
func Argv(cfg *config.Config, host string, up bool) ([]string, error) {
	hc, ok := cfg.Hosts[host]
	if !ok {
		return nil, fmt.Errorf("no host %q", host)
	}
	file := filepath.Join(RenderDir(cfg), "compose."+host+".yaml")
	argv := append(append([]string{}, hc.Exec...), "compose", "-p", "fleet", "-f", file)
	if up {
		return append(argv, "up", "-d", "--remove-orphans"), nil
	}
	return append(argv, "down"), nil
}

// Compose brings the named hosts (none: every host) up or down, echoing each
// command to w before it runs. A host with nothing to run is skipped, unless
// it was named, which is an error.
func Compose(ctx context.Context, cfg *config.Config, up bool, hosts []string, w io.Writer, run Runner) error {
	named := len(hosts) > 0
	if !named {
		hosts = cfg.HostNames()
	}
	for _, h := range hosts {
		if _, ok := cfg.Hosts[h]; !ok {
			return fmt.Errorf("no host %q", h)
		}
		if !render.Runs(cfg, h) {
			if named {
				return fmt.Errorf("%s has nothing to run", h)
			}
			continue
		}
		argv, err := Argv(cfg, h, up)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "$", strings.Join(argv, " "))
		if err := run(ctx, argv, w); err != nil {
			return fmt.Errorf("%s: %w", h, err)
		}
	}
	return nil
}
