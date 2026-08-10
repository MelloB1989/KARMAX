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
	return "karmax.yaml"
}
