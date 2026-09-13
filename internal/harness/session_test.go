package harness

import (
	"slices"
	"testing"
)

func TestSpawnArgsCarryMCPConfigAndPluginDir(t *testing.T) {
	got := extraArgs(Options{MCPConfig: `{"mcpServers":{}}`, PluginDir: "/tmp/skills"})
	want := []string{"--mcp-config", `{"mcpServers":{}}`, "--plugin-dir", "/tmp/skills"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if len(extraArgs(Options{})) != 0 {
		t.Error("empty options must add no arguments")
	}
}

// Regression guard: --mcp-config is variadic (see claude_code.go's browserArgs)
// and a future reorder must not put it ahead of an earlier flag.
func TestSpawnArgsPlaceExtrasAfterPartialMessages(t *testing.T) {
	s := &Session{ID: "sess-1", MCPConfig: `{"mcpServers":{}}`, PluginDir: "/tmp/skills"}
	args := spawnArgs(s, false, "")

	partial := slices.Index(args, "--include-partial-messages")
	mcp := slices.Index(args, "--mcp-config")
	plugin := slices.Index(args, "--plugin-dir")
	if partial < 0 || mcp < 0 || plugin < 0 {
		t.Fatalf("expected all three flags present, got %v", args)
	}
	if mcp < partial || plugin < partial {
		t.Errorf("--mcp-config/--plugin-dir must come after --include-partial-messages, got %v", args)
	}
}
