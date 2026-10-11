// Package hostctl is the argv and the reporting behind `fleetctl render`,
// `up` and `down`, shared by the CLI on a fleet host and by fleetd's routes
// (which run them for a remote operator).
package hostctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/outbound"
	"github.com/MelloB1989/karmax/fleet/internal/recreate"
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

// Options are what `up` and `down` may be asked beyond the plain command.
type Options struct {
	// Run runs the compose command, its output going to the writer.
	Run Runner
	// Docker runs the read-only docker calls of the plan.
	Docker recreate.Runner
	// DryRun stops after the plan; WhenIdle recreates agents one at a time,
	// once States (agent -> fleetd state) says each is not working.
	DryRun, WhenIdle bool
	States           func(ctx context.Context) (map[string]string, error)
	Sleep            func(ctx context.Context, d time.Duration) error
	Every            time.Duration
}

// RealDocker runs argv and returns its stdout; a failure carries stderr.
func RealDocker(ctx context.Context, argv []string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return out, err
}

// Update brings the named hosts (none: every host) up or down, writing to w
// what it does: for up, first the containers it will recreate and why (the
// plan), then each command before it runs. A host with nothing to run is
// skipped, unless it was named, which is an error. It is the one function
// behind `fleetctl up|down` on a fleet host and fleetd's /v1/up and /v1/down.
func Update(ctx context.Context, cfg *config.Config, up bool, hosts []string, o Options, w io.Writer) error {
	explicit := len(hosts) > 0
	if !explicit {
		hosts = cfg.HostNames()
	}
	for _, h := range hosts {
		hc, ok := cfg.Hosts[h]
		if !ok {
			return fmt.Errorf("no host %q", h)
		}
		if !render.Runs(cfg, h) {
			if explicit {
				return fmt.Errorf("%s has nothing to run", h)
			}
			continue
		}
		file := filepath.Join(RenderDir(cfg), "compose."+h+".yaml")
		pl := recreate.Planner{Prefix: hc.Exec, File: file, Run: o.Docker}
		var changes []recreate.Change
		if up {
			var err error
			if changes, err = pl.Plan(ctx); err != nil {
				return fmt.Errorf("%s: %w", h, err)
			}
			recreate.Describe(w, h, changes)
		}
		if o.DryRun {
			continue // the plan above is all a dry run does
		}
		if o.WhenIdle {
			agents := map[string]bool{}
			for _, n := range cfg.AgentsOn(h) {
				agents[n] = true
			}
			idle := recreate.Idler{Planner: pl, Agents: agents, State: o.States, Sleep: o.Sleep, Every: o.Every, Out: w}
			if err := idle.Up(ctx, changes); err != nil {
				return fmt.Errorf("%s: %w", h, err)
			}
			continue
		}
		argv, err := Argv(cfg, h, up)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "$", strings.Join(argv, " "))
		if err := o.Run(ctx, argv, w); err != nil {
			return fmt.Errorf("%s: %w", h, err)
		}
	}
	if o.DryRun {
		fmt.Fprintln(w, "dry run: nothing was recreated")
	}
	return nil
}
