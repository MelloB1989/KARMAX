// Package hostctl is the argv and the reporting behind `fleetctl render`,
// `up` and `down`, shared by the CLI on a fleet host and by fleetd's routes
// (which run them for a remote operator).
package hostctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/outbound"
	"github.com/MelloB1989/karmax/fleet/internal/recreate"
	"github.com/MelloB1989/karmax/fleet/internal/render"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// Runner runs one command, writing its output to w.
type Runner func(ctx context.Context, argv []string, w io.Writer) error

// ExecRunner runs the command for real.
func ExecRunner(ctx context.Context, argv []string, w io.Writer) error {
	c := target.HardenCtx(exec.CommandContext(ctx, argv[0], argv[1:]...))
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

// Dockerfile is the fleet image's recipe, relative to the repository root.
const Dockerfile = "packaging/fleet/Dockerfile"

// FindSource is the repository root holding the Dockerfile: start, or the
// nearest directory above it.
func FindSource(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, Dockerfile)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s at or above %s (run from the repository, or pass --src)", Dockerfile, start)
		}
		dir = parent
	}
}

// BuildArgv is the docker build of the fleet image, its identity taken from
// fleet.yaml — never typed by hand, since an image whose home is not the
// fleet's leaves every agent unable to write ~/.claude.json. An empty tag is
// fleet.yaml's image.
func BuildArgv(cfg *config.Config, src, tag string) ([]string, error) {
	if tag == "" {
		tag = cfg.Image
	}
	if !filepath.IsAbs(cfg.Home) || filepath.Clean(cfg.Home) == "/" {
		return nil, fmt.Errorf("fleet.yaml home %q must be an absolute path other than /", cfg.Home)
	}
	if cfg.User == "" {
		return nil, fmt.Errorf("no user name for the image: set user in fleet.yaml")
	}
	argv := []string{"docker", "build", "-f", filepath.Join(src, Dockerfile), "-t", tag,
		"--build-arg", fmt.Sprintf("UID=%d", cfg.UID),
		"--build-arg", fmt.Sprintf("GID=%d", cfg.GID),
		"--build-arg", "USER_NAME=" + cfg.User,
		"--build-arg", "HOME_DIR=" + cfg.Home,
	}
	if cfg.ClaudeVersion != "" {
		argv = append(argv, "--build-arg", "CLAUDE_VERSION="+cfg.ClaudeVersion)
	}
	return append(argv, src), nil
}

// ImageBuild builds the image, echoing the command first.
func ImageBuild(ctx context.Context, cfg *config.Config, src, tag string, w io.Writer, run Runner) error {
	argv, err := BuildArgv(cfg, src, tag)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "$", strings.Join(argv, " "))
	return run(ctx, argv, w)
}

// Piper runs `from | to`, writing both commands' stderr to w.
type Piper func(ctx context.Context, from, to []string, w io.Writer) error

// syncWriter serialises writes to a writer that is not safe for concurrent use.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// ExecPiper runs the pipeline for real.
func ExecPiper(ctx context.Context, from, to []string, w io.Writer) error {
	src := exec.CommandContext(ctx, from[0], from[1:]...)
	dst := exec.CommandContext(ctx, to[0], to[1:]...)
	// Both commands write w from their own copy goroutines: one lock for it.
	sw := &syncWriter{w: w}
	src.Stderr, dst.Stdout, dst.Stderr = sw, sw, sw
	pipe, err := src.StdoutPipe()
	if err != nil {
		return err
	}
	dst.Stdin = pipe
	if err := dst.Start(); err != nil {
		return err
	}
	if err := src.Start(); err != nil {
		_ = dst.Process.Kill()
		_ = dst.Wait()
		return err
	}
	serr, derr := src.Wait(), dst.Wait()
	if serr != nil {
		return fmt.Errorf("%s: %w", strings.Join(from, " "), serr)
	}
	if derr != nil {
		return fmt.Errorf("%s: %w", strings.Join(to, " "), derr)
	}
	return nil
}

// ImageShip sends the image to a host's engine: docker save | docker
// --context <host> load, with the host's own exec prefix. An empty tag is
// fleet.yaml's image.
func ImageShip(ctx context.Context, cfg *config.Config, host, tag string, w io.Writer, pipe Piper) error {
	hc, ok := cfg.Hosts[host]
	if !ok {
		return fmt.Errorf("no host %q", host)
	}
	if len(hc.Exec) == 0 {
		return fmt.Errorf("host %q has no exec prefix", host)
	}
	if tag == "" {
		tag = cfg.Image
	}
	from := []string{"docker", "save", tag}
	to := append(append([]string{}, hc.Exec...), "load")
	fmt.Fprintln(w, "$", strings.Join(from, " "), "|", strings.Join(to, " "))
	return pipe(ctx, from, to, w)
}
