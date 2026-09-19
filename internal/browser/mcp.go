package browser

import (
	"context"
	"encoding/json"
	"fmt"
)

// The browser, handed to a coding harness.
//
// Claude Code and Codex both reach a browser through the Playwright MCP server,
// and that server can either launch a browser of its own or attach to one over
// the DevTools protocol. Attaching is the whole point here: a browser it
// launched is signed into nothing, and the operator's sessions are in the
// window they signed into.
//
// The configuration is passed per invocation (`--mcp-config`), never written
// into the person's Claude Code settings. A browser tool that exists only for
// the length of one setup task is a smaller thing to have granted than one that
// is quietly present in every conversation afterwards.

// MCPServer is one entry in a harness's MCP configuration.
type MCPServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// MCPConfig is the shape both Claude Code and Codex read.
type MCPConfig struct {
	MCPServers map[string]MCPServer `json:"mcpServers"`
}

// MCPConfigJSON returns the --mcp-config value that points a harness at this
// browser, or an error when it is not running.
//
// Pinned to a version rather than @latest: what an agent is allowed to do to
// somebody's logged-in browser should not change because a release went out
// overnight.
func (s *Session) MCPConfigJSON(ctx context.Context) (string, error) {
	endpoint := s.Endpoint(ctx)
	if endpoint == "" {
		return "", ErrNotRunning
	}
	cfg := MCPConfig{MCPServers: map[string]MCPServer{
		"playwright": {
			Command: "npx",
			Args: []string{
				"-y", "@playwright/mcp@" + playwrightMCPVersion,
				"--cdp-endpoint", endpoint,
			},
		},
	}}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("browser mcp config: %w", err)
	}
	return string(b), nil
}

// The Playwright MCP release this has been tried against.
const playwrightMCPVersion = "0.0.80"
