package harness

import (
	"encoding/json"
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

// --effort is appended only when set, right after --model — mirroring how
// --fallback-model is conditional on the CLI's own degradation flag below it.
func TestSpawnArgsCarryEffort(t *testing.T) {
	args := spawnArgs(&Session{ID: "s", Model: "sonnet", Effort: "high"}, false, "")
	model := slices.Index(args, "--model")
	effort := slices.Index(args, "--effort")
	if model < 0 || effort < 0 {
		t.Fatalf("expected both --model and --effort, got %v", args)
	}
	if args[effort+1] != "high" {
		t.Errorf("effort value = %q, want high", args[effort+1])
	}
	if effort < model {
		t.Errorf("--effort must come after --model, got %v", args)
	}
}

func TestSpawnArgsOmitEffortWhenUnset(t *testing.T) {
	args := spawnArgs(&Session{ID: "s", Model: "sonnet"}, false, "")
	if slices.Contains(args, "--effort") {
		t.Errorf("empty effort must add no flag, got %v", args)
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

func TestAToolResultFlaggedAsErrorMarksItsCallFailed(t *testing.T) {
	var turn Turn
	mk := func(s string) event {
		var ev event
		if err := json.Unmarshal([]byte(s), &ev); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	turn.absorb(mk(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"ls"}},{"type":"tool_use","id":"b","name":"Bash","input":{"command":"karmax tool call comms.send"}}]}}`))
	turn.absorb(mk(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b","is_error":true,"content":"Exit code 1"},{"type":"tool_result","tool_use_id":"a","content":"ok"}]}}`))
	if turn.ToolCalls[0].Failed || !turn.ToolCalls[1].Failed {
		t.Fatalf("Failed flags wrong: %+v", turn.ToolCalls)
	}
}
