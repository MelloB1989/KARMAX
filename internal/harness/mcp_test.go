package harness

import (
	"slices"
	"testing"
)

// A harness session starts every MCP server the operator's own Claude config
// and plugins name unless it is told otherwise; with OnlyMCPConfig set it is
// told, and a per-session server still joins the allowed set.
func TestOnlyMCPConfigMakesTheSessionStrict(t *testing.T) {
	const only = `{"mcpServers":{"fleet":{"command":"fleetctl","args":["mcp"]}}}`
	const extra = `{"mcpServers":{"browser":{"command":"x"}}}`

	args := spawnArgs(&Session{ID: "s", OnlyMCPConfig: only, MCPConfig: extra}, false, "")
	i := slices.Index(args, "--strict-mcp-config")
	if i < 0 || i+2 >= len(args) || args[i+1] != "--mcp-config" || args[i+2] != only {
		t.Fatalf("want --strict-mcp-config --mcp-config <only>, got %q", args)
	}
	if !slices.Contains(args, extra) {
		t.Fatalf("the per-session config was dropped: %q", args)
	}

	// A kind that inherits the operator's servers is never strict, allowlist
	// or not; that is the one way back to the whole Claude Code config.
	if slices.Contains(spawnArgs(&Session{ID: "s", InheritMCP: true, OnlyMCPConfig: only}, false, ""), "--strict-mcp-config") {
		t.Fatal("a session that inherits the operator's MCP servers was made strict")
	}
}
