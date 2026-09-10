// Package hostpaths resolves the external binaries and directories KARMAX
// shells out to (wacli, gog, its own CLI, the default working dir). Nothing is
// hardcoded to a specific user: every path resolves via, in order,
//  1. an explicit environment variable (set it in .env to override),
//  2. a PATH lookup,
//  3. well-known locations relative to the current user's home.
//
// Every resolver is memoized — these are called on hot paths (per tool call)
// and the answers don't change while the daemon runs.
package hostpaths

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var (
	wacliOnce   sync.Once
	wacliPath   string
	gogOnce     sync.Once
	gogPath     string
	browserOnce sync.Once
	browserPath string
	binOnce     sync.Once
	binPath     string
	workOnce    sync.Once
	workDir     string
	wacliAPIMu  sync.Once
	wacliAPI    string
)

// Wacli returns the wacli binary path: $KARMAX_WACLI_PATH, then PATH, then
// ~/code/wacli/wacli. Returns "wacli" as a last resort (so error messages from
// exec name the missing binary rather than an empty string).
func Wacli() string {
	wacliOnce.Do(func() {
		wacliPath = resolve("KARMAX_WACLI_PATH", "wacli", "code/wacli/wacli")
	})
	return wacliPath
}

// Gog returns the Google Workspace CLI path: $KARMAX_GOG_PATH, then PATH, then
// the places a Go install and a Homebrew install put it.
//
// gogcli is the only Google CLI KARMAX knows about. The gws it replaced could
// only do interactive browser OAuth against a Workspace whose reauth policy
// logs an unattended process out every few hours — see
// internal/tools/builtin/gog.go. It is also one fewer runtime on the host: gws
// was an npm package and needed Node, while gogcli ships a static binary per
// platform and builds with `go install`.
func Gog() string {
	gogOnce.Do(func() {
		gogPath = resolve("KARMAX_GOG_PATH", "gog", "go/bin/gog", ".local/bin/gog")
	})
	return gogPath
}

// Browser returns a Chromium-family browser this machine already has:
// $KARMAX_BROWSER_PATH, then PATH, then where each platform installs Chrome,
// Chromium and Edge. Empty when there is none.
//
// Chromium-family specifically, because the agent reaches the browser over the
// DevTools protocol and Firefox and Safari do not speak it. Empty rather than a
// bare name, because "no browser on this machine" is a thing the caller has to
// tell somebody about, not a command to fail at later.
func Browser() string {
	browserOnce.Do(func() { browserPath = findBrowser() })
	return browserPath
}

func findBrowser() string {
	if v := strings.TrimSpace(os.Getenv("KARMAX_BROWSER_PATH")); v != "" {
		return v
	}
	for _, name := range []string{
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser",
		"microsoft-edge", "microsoft-edge-stable", "brave-browser",
	} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	home, _ := os.UserHomeDir()
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		for _, app := range []string{
			"Google Chrome.app/Contents/MacOS/Google Chrome",
			"Chromium.app/Contents/MacOS/Chromium",
			"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"Brave Browser.app/Contents/MacOS/Brave Browser",
		} {
			candidates = append(candidates, filepath.Join("/Applications", app))
			if home != "" {
				candidates = append(candidates, filepath.Join(home, "Applications", app))
			}
		}
	case "windows":
		for _, root := range []string{
			os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA"),
		} {
			if root == "" {
				continue
			}
			candidates = append(candidates,
				filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(root, "Chromium", "Application", "chrome.exe"),
			)
		}
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// KarmaxBin returns the karmax CLI path that delegated harnesses (Claude Code)
// shell out to: $KARMAX_BIN, then the currently running executable, then PATH.
func KarmaxBin() string {
	binOnce.Do(func() {
		if v := strings.TrimSpace(os.Getenv("KARMAX_BIN")); v != "" {
			binPath = v
			return
		}
		if exe, err := os.Executable(); err == nil {
			// Resolve symlinks so `karmax` restarted from a rebuilt binary still
			// points at a real file.
			if r, err := filepath.EvalSymlinks(exe); err == nil {
				exe = r
			}
			binPath = exe
			return
		}
		if p, err := exec.LookPath("karmax"); err == nil {
			binPath = p
			return
		}
		binPath = "karmax"
	})
	return binPath
}

// WorkDir returns the default working directory for delegated coding tasks:
// $KARMAX_WORKDIR, then the user's home, then ".".
func WorkDir() string {
	workOnce.Do(func() {
		if v := strings.TrimSpace(os.Getenv("KARMAX_WORKDIR")); v != "" {
			workDir = v
			return
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			workDir = home
			return
		}
		workDir = "."
	})
	return workDir
}

// WacliAPIURL returns the base URL of the local wacli HTTP API:
// $KARMAX_WACLI_API_URL, defaulting to wacli's standard localhost port.
func WacliAPIURL() string {
	wacliAPIMu.Do(func() {
		wacliAPI = strings.TrimSpace(os.Getenv("KARMAX_WACLI_API_URL"))
		if wacliAPI == "" {
			wacliAPI = "http://127.0.0.1:8765"
		}
		wacliAPI = strings.TrimRight(wacliAPI, "/")
	})
	return wacliAPI
}

// resolve implements the shared env → PATH → home-relative resolution order.
// homeRelative entries are joined to the user's home directory and used if the
// file exists. Falls back to the bare command name.
func resolve(envVar, command string, homeRelative ...string) string {
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		return v
	}
	if p, err := exec.LookPath(command); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, rel := range homeRelative {
			p := filepath.Join(home, rel)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return command
}
