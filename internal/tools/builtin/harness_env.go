package builtin

import (
	"os"
	"strings"
)

// harnessEnv returns the current environment with KARMAX's gateway/Anthropic
// auth variables removed, so spawned coding harnesses (claude, codex) use their
// OWN authentication (e.g. the claude.ai login) instead of being redirected to
// KARMAX's local model gateway — which speaks a different request format and
// breaks features like web search / connectors.
func harnessEnv() []string {
	strip := map[string]bool{
		"ANTHROPIC_API_KEY":          true,
		"ANTHROPIC_AUTH_TOKEN":       true,
		"ANTHROPIC_BASE_URL":         true,
		"ANTHROPIC_MODEL":            true,
		"ANTHROPIC_SMALL_FAST_MODEL": true,
	}
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if strip[key] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
