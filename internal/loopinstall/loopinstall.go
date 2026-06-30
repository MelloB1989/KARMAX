// Package loopinstall manages installed loopkit loop modules: it edits the
// blank-import registry file, fetches modules, rebuilds KARMAX, and restarts
// the service. The `karmax loops` TUI drives it.
package loopinstall

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const registryRel = "internal/installedloops/installed.go"

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

// InstalledModules returns the loop module import paths in the registry file.
func InstalledModules(root string) ([]string, error) {
	f, err := os.Open(filepath.Join(root, registryRel))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var mods []string
	inBlock := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.Contains(line, "karmax:loops:begin"):
			inBlock = true
		case strings.Contains(line, "karmax:loops:end"):
			inBlock = false
		case inBlock && strings.HasPrefix(line, `_ "`):
			mods = append(mods, strings.Trim(strings.TrimPrefix(line, "_ "), `"`))
		}
	}
	return mods, sc.Err()
}

// Install fetches a loop module, blank-imports it, and rebuilds KARMAX.
func Install(root, module string) (string, error) {
	module = strings.TrimSpace(module)
	if module == "" {
		return "", fmt.Errorf("empty module path")
	}
	if strings.Contains(module, " ") {
		return "", fmt.Errorf("invalid module path %q", module)
	}
	var log strings.Builder
	goBin := GoBin()
	step := func(label, name string, args ...string) error {
		fmt.Fprintf(&log, "$ %s %s\n", name, strings.Join(args, " "))
		out, err := run(root, name, args...)
		log.WriteString(out)
		if err != nil {
			return fmt.Errorf("%s failed: %w", label, err)
		}
		return nil
	}
	if err := step("go get", goBin, "get", module); err != nil {
		return log.String(), err
	}
	if err := addImport(root, module); err != nil {
		return log.String(), err
	}
	if err := step("go mod tidy", goBin, "mod", "tidy"); err != nil {
		return log.String(), err
	}
	if err := rebuild(root, &log); err != nil {
		// roll back the import so a broken module doesn't wedge future builds
		_ = removeImport(root, module)
		_, _ = run(root, "go", "mod", "tidy")
		return log.String(), err
	}
	return log.String(), nil
}

// Remove drops a loop module's import and rebuilds.
func Remove(root, module string) (string, error) {
	var log strings.Builder
	if err := removeImport(root, module); err != nil {
		return "", err
	}
	fmt.Fprintf(&log, "$ go mod tidy\n")
	if out, err := run(root, GoBin(), "mod", "tidy"); err != nil {
		log.WriteString(out)
	}
	if err := rebuild(root, &log); err != nil {
		return log.String(), err
	}
	return log.String(), nil
}

// Restart restarts the KARMAX systemd user service so the new binary takes over.
func Restart() (string, error) {
	return run("", "systemctl", "--user", "restart", "karmax.service")
}

func rebuild(root string, log *strings.Builder) error {
	fmt.Fprintf(log, "$ CGO_ENABLED=1 %s build -o karmax ./cmd/karmax\n", GoBin())
	out, err := runEnv(root, []string{"CGO_ENABLED=1"}, GoBin(), "build", "-o", "karmax", "./cmd/karmax")
	log.WriteString(out)
	if err != nil {
		return fmt.Errorf("rebuild failed: %w", err)
	}
	// On a binary-only install (workspace != running binary), replace the
	// running binary with the freshly built one.
	if err := swapBinary(root); err != nil {
		return fmt.Errorf("rebuild ok but binary swap failed: %w", err)
	}
	return nil
}

func addImport(root, module string) error {
	path := filepath.Join(root, registryRel)
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := string(b)
	if strings.Contains(content, `"`+module+`"`) {
		return nil // already imported
	}
	idx := strings.Index(content, "karmax:loops:end")
	if idx < 0 {
		return fmt.Errorf("end marker not found in %s", registryRel)
	}
	lineStart := strings.LastIndex(content[:idx], "\n") + 1
	insert := "\t_ \"" + module + "\"\n"
	return os.WriteFile(path, []byte(content[:lineStart]+insert+content[lineStart:]), 0644)
}

func removeImport(root, module string) error {
	path := filepath.Join(root, registryRel)
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	target := `"` + module + `"`
	var kept []string
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, `_ "`) && strings.Contains(ln, target) {
			continue
		}
		kept = append(kept, ln)
	}
	return os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0644)
}

func run(dir, name string, args ...string) (string, error) {
	return runEnv(dir, nil, name, args...)
}

func runEnv(dir string, extraEnv []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
