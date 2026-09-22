package instagram

// Getting the helper's Python environment onto the machine, the first time
// somebody actually asks for it.
//
// Not at install time and not at boot: KARMAX is one static binary, most
// installs will never enable this connector, and none of them should pay for a
// Python download to find that out. The cost lands on the first call, once,
// and every call after it finds the environment already there.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// installTimeout bounds provisioning. It can mean downloading uv, a Python
// interpreter and a dependency tree on a slow connection, so it is minutes
// rather than seconds — but it is bounded, because an install that hangs
// forever looks identical to one that is merely slow.
const installTimeout = 10 * time.Minute

func logf(format string, args ...any) {
	zap.L().Info(fmt.Sprintf(format, args...))
}

// provisioning is the one install in flight, shared by every caller.
var (
	provisionMu sync.Mutex
	provisionAt *provisionRun
)

type provisionRun struct {
	done   chan struct{}
	python string
	err    error
}

// errInstalling is what a caller that gives up on a first-time install hears.
var errInstalling = errors.New("instagram: still setting up the helper — the first use downloads " +
	"Python and instagrapi, which takes a minute or two. Try again shortly")

// provision is ensureInstalled on its own clock. The install runs under its
// own timeout rather than the caller's, because the first callers are short —
// a 25-second health check, a harness shell command — and one of them giving
// up must not kill a half-built venv and send the next caller back to the
// start of the download.
func provision(ctx context.Context, dir string) (string, error) {
	provisionMu.Lock()
	run := provisionAt
	if run == nil {
		run = &provisionRun{done: make(chan struct{})}
		provisionAt = run
		go func() {
			run.python, run.err = ensureInstalled(context.Background(), dir)
			provisionMu.Lock()
			provisionAt = nil
			provisionMu.Unlock()
			close(run.done)
		}()
	}
	provisionMu.Unlock()

	select {
	case <-run.done:
		return run.python, run.err
	case <-ctx.Done():
		return "", errInstalling
	}
}

// Prepare installs the helper's environment without signing in or touching
// Instagram, so the first real call does not pay for the download.
func (c *Connector) Prepare(ctx context.Context) error {
	dir, err := helperDir()
	if err != nil {
		return err
	}
	_, err = provision(ctx, dir)
	return err
}

// ensureInstalled returns the interpreter to run the helper with, provisioning
// it if this machine does not have one yet.
func ensureInstalled(ctx context.Context, dir string) (string, error) {
	// An operator who would rather build the environment themselves — a
	// different Python, a vetted mirror, an air-gapped box — points at it and
	// nothing here runs.
	if p := strings.TrimSpace(os.Getenv("KARMAX_INSTAGRAM_PYTHON_PATH")); p != "" {
		if usable(ctx, p) {
			return p, nil
		}
		return "", fmt.Errorf("instagram: KARMAX_INSTAGRAM_PYTHON_PATH is set to %q, "+
			"but that interpreter cannot import instagrapi", p)
	}

	venv := filepath.Join(dir, "venv", "bin", "python")
	if v := strings.TrimSpace(os.Getenv("KARMAX_INSTAGRAM_VENV")); v != "" {
		venv = filepath.Join(v, "bin", "python")
	}
	if usable(ctx, venv) {
		return venv, nil
	}

	if os.Getenv("KARMAX_INSTAGRAM_NO_FETCH") == "1" {
		return "", fmt.Errorf("instagram: the helper is not installed and fetching is " +
			"turned off — install instagrapi yourself and set KARMAX_INSTAGRAM_PYTHON_PATH")
	}

	script, err := materialise(dir, "install.sh", installSource, 0o700)
	if err != nil {
		return "", fmt.Errorf("instagram: could not write the installer: %w", err)
	}

	logf("instagram: installing the helper (first use; this takes a minute)")
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", script)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		// The script says why on stderr, and that reason is the useful part —
		// "exit status 3" on its own tells an operator nothing to act on.
		return "", fmt.Errorf("instagram: could not install the helper: %w — %s",
			err, lastLines(errOut.String(), 3))
	}
	for _, line := range strings.Split(errOut.String(), "\n") {
		if line != "" {
			logf("instagram: %s", line)
		}
	}

	// The script prints the interpreter it built. Trust it over guessing the
	// path again, since KARMAX_INSTAGRAM_VENV may have moved it.
	python := strings.TrimSpace(lastLines(out.String(), 1))
	if python == "" {
		python = venv
	}
	if !usable(ctx, python) {
		return "", fmt.Errorf("instagram: the installer finished but %q still "+
			"cannot import instagrapi", python)
	}
	return python, nil
}

// usable reports whether this interpreter exists and has instagrapi in it.
//
// Importing rather than checking the file exists: a venv left half-built by an
// interrupted install has the interpreter and not the package, and that
// difference only shows up as a confusing error much later.
func usable(ctx context.Context, python string) bool {
	if python == "" {
		return false
	}
	if _, err := os.Stat(python); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, python, "-c", "import instagrapi").Run() == nil
}

func lastLines(s string, n int) string {
	lines := []string{}
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}
