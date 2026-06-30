// Command karmax is the CLI + daemon entrypoint for the KARMAX agent harness.
// The command tree is built with cobra; each command group lives in its own
// file (runtime_cmd.go, config_cmd.go, api_cmd.go, loops_tui.go) and is wired
// up in root.go.
package main

import (
	"os"
	"path/filepath"

	_ "github.com/MelloB1989/karmax/internal/installedloops" // third-party loopkit loops (managed by `karmax loops`)
	_ "github.com/MelloB1989/karmax/internal/loops/core"     // built-in loopkit loops (migrated from karmax.yaml)
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
	candidates := []string{"karmax.yaml", "karmax.yml"}
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
