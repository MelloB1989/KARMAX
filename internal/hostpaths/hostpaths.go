// Package hostpaths resolves the external binaries and directories KARMAX
// shells out to (wacli, gws, its own CLI, the default working dir). Nothing is
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
	"strings"
	"sync"
)

var (
	wacliOnce  sync.Once
	wacliPath  string
	gwsOnce    sync.Once
	gwsPath    string
	binOnce    sync.Once
	binPath    string
	workOnce   sync.Once
	workDir    string
	wacliAPIMu sync.Once
	wacliAPI   string
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

// GWS returns the Google Workspace CLI path: $KARMAX_GWS_PATH, then PATH, then
// ~/.hermes/node/bin/gws and ~/.local/bin/gws.
func GWS() string {
	gwsOnce.Do(func() {
		gwsPath = resolve("KARMAX_GWS_PATH", "gws", ".hermes/node/bin/gws", ".local/bin/gws")
	})
	return gwsPath
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
