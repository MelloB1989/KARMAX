// Package loopinstall manages installed loopkit loop modules: it edits the
// blank-import registry file, fetches modules, rebuilds KARMAX, and restarts
// the service. The `karmax loops` TUI drives it.
package loopinstall

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// DataDir is KARMAX's data directory (state + disabled-loops list). Honors
// $KARMAX_DATA_DIR, else ~/.karmax.
func DataDir() string {
	if d := strings.TrimSpace(os.Getenv("KARMAX_DATA_DIR")); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".karmax")
}

func disabledLoopsPath() string { return filepath.Join(DataDir(), "loops-disabled.txt") }

// LoadDisabledLoops returns the set of loop names the operator has disabled.
// Disabling happens at the runtime level (the loop isn't scheduled) — no rebuild
// required, and it works for built-in and installed loops alike.
func LoadDisabledLoops() map[string]bool {
	set := map[string]bool{}
	b, err := os.ReadFile(disabledLoopsPath())
	if err != nil {
		return set
	}
	for _, line := range strings.Split(string(b), "\n") {
		name := strings.TrimSpace(line)
		if name != "" && !strings.HasPrefix(name, "#") {
			set[name] = true
		}
	}
	return set
}

// SetLoopDisabled toggles whether a loop (by name) is disabled and persists it.
func SetLoopDisabled(name string, disabled bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty loop name")
	}
	set := LoadDisabledLoops()
	if disabled {
		set[name] = true
	} else {
		delete(set, name)
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	if err := os.MkdirAll(DataDir(), 0755); err != nil {
		return err
	}
	body := "# Loops disabled by the operator (one name per line). Managed by `karmax loops`.\n"
	if len(names) > 0 {
		body += strings.Join(names, "\n") + "\n"
	}
	return os.WriteFile(disabledLoopsPath(), []byte(body), 0644)
}

// RepoRoot locates the KARMAX source module root (the dir with a go.mod whose
// module is github.com/MelloB1989/karmax). It checks $KARMAX_SRC, then walks up
// from the executable's dir, then from the working directory.

// run executes a command, returning its combined output on failure so the
// caller can say what went wrong rather than that something did.
func run(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// RepoRoot locates the KARMAX source module root (the dir with a go.mod whose
// module is github.com/MelloB1989/karmax). It checks $KARMAX_SRC, then walks up
// from the executable's dir, then from the working directory.
func RepoRoot() (string, error) {
	var starts []string
	if env := strings.TrimSpace(os.Getenv("KARMAX_SRC")); env != "" {
		starts = append(starts, env)
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			starts = append(starts, filepath.Dir(real))
		}
		starts = append(starts, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	starts = append(starts, WorkspaceDir()) // managed clone (binary-only installs)
	for _, start := range starts {
		for dir := start; ; {
			b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
			if err == nil && strings.Contains(string(b), "module github.com/MelloB1989/karmax") {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("no KARMAX source found — run `karmax setup` to clone it, or set KARMAX_SRC=/path/to/KARMAX")
}
