package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const (
	defaultMemoryLimit = "2g"
	defaultCPULimit    = "2"
)

// DockerDriver runs sandboxes as containers on a local Docker daemon by
// shelling out to the `docker` CLI — no SDK dependency, so it fails exactly
// the way an operator's own `docker` command would.
type DockerDriver struct {
	// MemoryLimit and CPULimit are docker run resource flags (e.g. "2g", "2").
	// Set from KARMAX_SANDBOX_MEMORY / KARMAX_SANDBOX_CPUS by NewDockerDriver;
	// exported so a caller can override them directly (tests do).
	MemoryLimit string
	CPULimit    string
}

// NewDockerDriver checks that `docker` actually works before handing back a
// driver — better an operator with no Docker gets one sentence here than a
// mystery three steps later inside Launch.
func NewDockerDriver() (*DockerDriver, error) {
	out, err := exec.Command("docker", "version").CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("sandbox: docker CLI not found on PATH — install Docker or point KARMAX at a host that has it")
		}
		return nil, fmt.Errorf("sandbox: docker is not usable (is the daemon running?): %s", strings.TrimSpace(string(out)))
	}
	d := &DockerDriver{MemoryLimit: defaultMemoryLimit, CPULimit: defaultCPULimit}
	if v := strings.TrimSpace(os.Getenv("KARMAX_SANDBOX_MEMORY")); v != "" {
		d.MemoryLimit = v
	}
	if v := strings.TrimSpace(os.Getenv("KARMAX_SANDBOX_CPUS")); v != "" {
		d.CPULimit = v
	}
	return d, nil
}

func (d *DockerDriver) Name() string { return "docker" }

// Launch starts a detached container and returns its id. Credentials in
// Spec.Env never touch argv — argv is world-readable via /proc on the host,
// and these are tokens — they go into a 0600 temp file consumed by
// --env-file. Task/Repo/Branch ride the same file: task text can carry
// sensitive context too, and the file mechanism is already there.
func (d *DockerDriver) Launch(ctx context.Context, s Spec) (string, error) {
	if s.Image == "" {
		return "", fmt.Errorf("sandbox: spec.Image is required")
	}
	if s.Repo == "" {
		return "", fmt.Errorf("sandbox: spec.Repo is required")
	}

	env := make(map[string]string, len(s.Env)+3)
	for k, v := range s.Env {
		env[k] = v
	}
	env["TASK"] = s.Task
	env["REPO"] = s.Repo
	env["BASE_BRANCH"] = s.Branch

	envFile, err := writeEnvFile(env)
	if err != nil {
		return "", fmt.Errorf("sandbox: env file: %w", err)
	}
	// Only needs to survive docker reading it at container start, not the
	// container's lifetime.
	defer os.Remove(envFile)

	runID := uuid.New().String()
	args := runArgs(s.Image, envFile, runID, d.MemoryLimit, d.CPULimit)

	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return "", fmt.Errorf("sandbox: docker run: %w", explainExecErr(err))
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("sandbox: docker run returned no container id")
	}
	return id, nil
}

// runArgs is factored out of Launch so tests can pin its shape: it never
// takes a raw env value as a parameter, only the path of a file already
// holding them, so there is no argument through which a secret could reach
// argv.
func runArgs(image, envFile, runID, memLimit, cpuLimit string) []string {
	return []string{
		"run", "-d",
		"--rm=false",
		"--memory", memLimit,
		"--cpus", cpuLimit,
		"--env-file", envFile,
		"--label", "karmax.sandbox=1",
		"--label", "karmax.run-id=" + runID,
		"--name", "karmax-sandbox-" + runID,
		image,
	}
}

// writeEnvFile puts env in a docker --env-file this driver owns exclusively
// while it's on disk: created 0600 before anything is written to it.
func writeEnvFile(env map[string]string) (string, error) {
	content, err := envFileContent(env)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "karmax-sandbox-env-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// envFileContent renders env as KEY=VALUE lines. A newline in either half
// would split into a bogus extra line docker would parse as its own
// variable, so that's rejected rather than silently mangled.
func envFileContent(env map[string]string) (string, error) {
	var buf bytes.Buffer
	for k, v := range env {
		if strings.ContainsAny(k, "=\n") || strings.Contains(v, "\n") {
			return "", fmt.Errorf("env var %q is not representable in a docker --env-file line", k)
		}
		buf.WriteString(k)
		buf.WriteByte('=')
		buf.WriteString(v)
		buf.WriteByte('\n')
	}
	return buf.String(), nil
}

// dockerInspect is the slice of `docker inspect` we read; the real payload
// carries much more, but this is what Status needs.
type dockerInspect struct {
	State struct {
		Status    string `json:"Status"`
		ExitCode  int    `json:"ExitCode"`
		OOMKilled bool   `json:"OOMKilled"`
	} `json:"State"`
}

// Poll maps `docker inspect` onto the driver's state vocabulary. A container
// Docker has never heard of is StateGone, not an error — killed by hand,
// reaped by the host, or lost with the machine.
func (d *DockerDriver) Poll(ctx context.Context, id string) (Status, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", id).Output()
	if err != nil {
		if isGoneErr(err) {
			return Status{ID: id, State: StateGone}, nil
		}
		return Status{}, fmt.Errorf("sandbox: docker inspect: %w", explainExecErr(err))
	}
	var entries []dockerInspect
	if err := json.Unmarshal(out, &entries); err != nil {
		return Status{}, fmt.Errorf("sandbox: docker inspect: parse: %w", err)
	}
	if len(entries) == 0 {
		return Status{ID: id, State: StateGone}, nil
	}
	return statusFromInspect(id, entries[0]), nil
}

func statusFromInspect(id string, e dockerInspect) Status {
	st := Status{ID: id, ExitCode: e.State.ExitCode}
	switch e.State.Status {
	case "created":
		st.State = StateStarting
	case "running", "restarting", "paused":
		st.State = StateRunning
	case "removing":
		// Mid-teardown: treat like exited rather than gone since Docker still
		// has an exit code on record for it.
		st.State = StateExited
	case "exited":
		if e.State.ExitCode == 0 && !e.State.OOMKilled {
			st.State = StateExited
		} else {
			st.State = StateFailed
		}
	case "dead":
		st.State = StateFailed
	default:
		st.State = StateFailed
	}
	return st
}

// Logs shells `docker logs --tail N`. Container stdout/stderr are merged —
// fine for a tail, and there's no ordering guarantee worth preserving one
// buffer over the other for.
func (d *DockerDriver) Logs(ctx context.Context, id string, tail int) (string, error) {
	if tail <= 0 {
		tail = 200
	}
	cmd := exec.CommandContext(ctx, "docker", "logs", "--tail", strconv.Itoa(tail), id)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		if isGoneMsg(buf.String()) {
			return "", nil
		}
		return "", fmt.Errorf("sandbox: docker logs: %w", err)
	}
	return buf.String(), nil
}

// Kill stops and removes in one call; a container already gone is success,
// not an error — the caller wanted it gone and it is.
func (d *DockerDriver) Kill(ctx context.Context, id string) error {
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", id).CombinedOutput()
	if err != nil && !isGoneMsg(string(out)) {
		return fmt.Errorf("sandbox: docker rm: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func isGoneMsg(s string) bool {
	return strings.Contains(strings.ToLower(s), "no such")
}

func isGoneErr(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && isGoneMsg(string(ee.Stderr))
}

// explainExecErr surfaces the docker CLI's own stderr instead of just
// "exit status 1", which is all *exec.ExitError says on its own.
func explainExecErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return errors.New(strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}
