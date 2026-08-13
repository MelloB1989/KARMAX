// Command karmax is the CLI + daemon entrypoint for the KARMAX agent harness.
// The command tree is built with cobra; each command group lives in its own
// file (runtime_cmd.go, config_cmd.go, api_cmd.go, loops_tui.go) and is wired
// up in root.go.
package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

// Version is overridable at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	Execute()
}

// loadDotEnv loads environment variables from a .env file (working directory
// first, then ~/.karmax/.env) so ${VAR} references in karmax.yaml expand and
// provider SDKs pick up credentials. Non-fatal; never overrides the real env.
func loadDotEnv() {
	_ = godotenv.Load()
	if home, err := os.UserHomeDir(); err == nil {
		_ = godotenv.Load(filepath.Join(home, ".karmax", ".env"))
	}
}

// findConfig resolves the karmax.yaml path: the --config flag wins, then the
// working directory, then ~/.karmax.
func findConfig() string {
	if cfgPath != "" {
		return cfgPath
	}
	var candidates []string
	// An explicit KARMAX_DATA_DIR wins over the working directory.
	//
	// It used to lose, and the consequence was severe: a second instance
	// started from the repo checkout silently loaded the FIRST instance's
	// karmax.yaml — same ports, same agent, and the operator's real WhatsApp
	// channel. Two instances then shared one identity while believing they
	// were separate. Setting a data dir means "this is my instance"; nothing
	// ambient should be able to override it.
	if d := strings.TrimSpace(os.Getenv("KARMAX_DATA_DIR")); d != "" {
		candidates = append(candidates,
			filepath.Join(d, "karmax.yaml"),
			filepath.Join(d, "karmax.yml"),
		)
	}
	candidates = append(candidates, "karmax.yaml", "karmax.yml")
	if home, _ := os.UserHomeDir(); home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".karmax", "karmax.yaml"),
			filepath.Join(home, ".karmax", "karmax.yml"),
		)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	// Last resort: the config a running instance recorded. Checked after every
	// explicit location so it can never override a deliberate local file — it
	// only answers "I am not in the checkout and there is no config here",
	// which is where the operator usually is.
	dataDir := strings.TrimSpace(os.Getenv("KARMAX_DATA_DIR"))
	if dataDir == "" {
		if home, _ := os.UserHomeDir(); home != "" {
			dataDir = filepath.Join(home, ".karmax")
		}
	}
	if p := configFromPointer(dataDir); p != "" {
		return p
	}
	return "karmax.yaml"
}

// configPointer is the file the running daemon leaves in its data dir naming
// the config it actually loaded.
const configPointer = "config-path"

// recordConfigPath notes where this instance's config lives, so the CLI works
// from any directory.
//
// The config is found by walking the working directory, which is right for the
// daemon — it is started from its own checkout — and wrong for the operator,
// who runs `karmax integrations` from wherever they happen to be and got
// "open karmax.yaml: no such file or directory" for their trouble.
//
// Written under the data dir rather than a fixed path, so an instance with its
// own KARMAX_DATA_DIR leaves its own pointer and two instances never learn each
// other's config.
func recordConfigPath(dataDir, cfgFile string) {
	dir := expandHome(strings.TrimSpace(dataDir))
	abs, err := filepath.Abs(cfgFile)
	if err != nil || dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	// Best effort throughout: failing to leave a convenience pointer is not a
	// reason to refuse to start.
	_ = os.WriteFile(filepath.Join(dir, configPointer), []byte(abs+"\n"), 0o644)
}

// configFromPointer reads the path a running instance recorded.
func configFromPointer(dataDir string) string {
	dir := expandHome(strings.TrimSpace(dataDir))
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, configPointer))
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(string(data))
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		// The checkout moved or was deleted; a stale pointer is worse than none.
		return ""
	}
	return path
}

// expandHome resolves a leading ~ so a data dir written as "~/.karmax" in YAML
// is usable as a path.
func expandHome(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
