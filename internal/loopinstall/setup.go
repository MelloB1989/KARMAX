package loopinstall

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// RepoURL is the public KARMAX repo, cloned (shallow) to rebuild the binary when
// installing loops on a machine that only has the binary (no dev checkout).
const RepoURL = "https://github.com/MelloB1989/KARMAX"

// WorkspaceDir is the managed source checkout used for rebuilds.
func WorkspaceDir() string { return filepath.Join(DataDir(), "src") }

func toolchainGo() string { return filepath.Join(DataDir(), "toolchain", "go", "bin", "go") }

// GoBin returns the Go binary to build with: the managed toolchain if present,
// otherwise the system "go".
func GoBin() string {
	if mg := toolchainGo(); goRunnable(mg) {
		return mg
	}
	return "go"
}

func goRunnable(bin string) bool {
	return exec.Command(bin, "version").Run() == nil
}

// HaveGo reports whether a usable Go compiler is available (system or managed).
func HaveGo() bool { return goRunnable("go") || goRunnable(toolchainGo()) }

// HaveGit reports whether git is available (needed to clone the workspace).
func HaveGit() bool { return exec.Command("git", "--version").Run() == nil }

// EnsureGo returns a working Go binary, installing the toolchain into
// ~/.karmax/toolchain if neither the system nor a managed Go is present.
func EnsureGo() (string, error) {
	if goRunnable("go") {
		return "go", nil
	}
	if mg := toolchainGo(); goRunnable(mg) {
		return mg, nil
	}
	return installGo()
}

func installGo() (string, error) {
	if !commandExists("curl") || !commandExists("tar") {
		return "", fmt.Errorf("need curl and tar to install Go automatically; install Go manually from https://go.dev/dl")
	}
	ver, err := latestGoVersion()
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://go.dev/dl/%s.%s-%s.tar.gz", ver, runtime.GOOS, runtime.GOARCH)
	tcDir := filepath.Join(DataDir(), "toolchain")
	if err := os.MkdirAll(tcDir, 0755); err != nil {
		return "", err
	}
	tmp := filepath.Join(os.TempDir(), "karmax-"+ver+".tar.gz")
	defer os.Remove(tmp)
	if out, err := run("", "curl", "-fSL", url, "-o", tmp); err != nil {
		return "", fmt.Errorf("download %s: %w\n%s", url, err, out)
	}
	_ = os.RemoveAll(filepath.Join(tcDir, "go"))
	if out, err := run("", "tar", "-C", tcDir, "-xzf", tmp); err != nil {
		return "", fmt.Errorf("extract Go: %w\n%s", err, out)
	}
	mg := toolchainGo()
	if !goRunnable(mg) {
		return "", fmt.Errorf("installed Go at %s is not runnable", mg)
	}
	return mg, nil
}

func latestGoVersion() (string, error) {
	out, err := run("", "curl", "-fsSL", "https://go.dev/VERSION?m=text")
	if err != nil {
		return "", fmt.Errorf("fetch latest Go version: %w", err)
	}
	ver := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if !strings.HasPrefix(ver, "go") {
		return "", fmt.Errorf("unexpected Go version response: %q", ver)
	}
	return ver, nil
}

// EnsureWorkspace returns a usable KARMAX source root: an existing dev checkout
// or managed clone if present, otherwise it clones the repo (shallow) to
// ~/.karmax/src.
func EnsureWorkspace() (string, error) {
	if root, err := RepoRoot(); err == nil {
		return root, nil
	}
	if !HaveGit() {
		return "", fmt.Errorf("git is required to clone the KARMAX source; install git or run from a checkout")
	}
	ws := WorkspaceDir()
	if err := os.MkdirAll(filepath.Dir(ws), 0755); err != nil {
		return "", err
	}
	if out, err := run("", "git", "clone", "--depth=1", RepoURL, ws); err != nil {
		return "", fmt.Errorf("clone %s: %w\n%s", RepoURL, err, out)
	}
	return ws, nil
}

// swapBinary replaces the currently-running karmax binary with the freshly built
// one when they differ (binary-only install). In a dev checkout the build writes
// the binary in place, so this is a no-op.
func swapBinary(root string) error {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	built := filepath.Join(root, "karmax")
	if built == exe {
		return nil
	}
	if _, err := os.Stat(built); err != nil {
		return fmt.Errorf("built binary not found at %s: %w", built, err)
	}
	return copyOverExe(built, exe)
}

func copyOverExe(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	return os.Rename(tmp, dst) // rename over a running binary is allowed on unix
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
