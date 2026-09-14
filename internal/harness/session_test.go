package harness

import (
	"slices"
	"testing"
)

func TestSpawnArgsCarryMCPConfig(t *testing.T) {
	got := extraArgs(Options{MCPConfig: `{"mcpServers":{}}`})
	want := []string{"--mcp-config", `{"mcpServers":{}}`}
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
	s := &Session{ID: "sess-1", MCPConfig: `{"mcpServers":{}}`}
	args := spawnArgs(s, false, "")

	partial := slices.Index(args, "--include-partial-messages")
	mcp := slices.Index(args, "--mcp-config")
	if partial < 0 || mcp < 0 {
		t.Fatalf("expected both flags present, got %v", args)
	}
	if mcp < partial {
		t.Errorf("--mcp-config must come after --include-partial-messages, got %v", args)
	}
}
