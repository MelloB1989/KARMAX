package harness

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// A session is spawned with only the MCP servers KARMAX hands it.
//
// Without --strict-mcp-config the CLI also starts every server in the
// operator's own Claude Code config. On the live machine that was three
// process trees per session (a 3D editor's bridge, the fleet's, a browser
// driver fetched from npm on every start) for sessions that never called one
// of them, and an unattended session holding a tool that drives the
// operator's open 3D scene.
func TestASessionGetsOnlyTheMCPServersKARMAXGivesIt(t *testing.T) {
	bare := spawnArgs(&Session{ID: "s"}, false, "")
	if !slices.Contains(bare, "--strict-mcp-config") {
		t.Errorf("args = %v, want --strict-mcp-config", bare)
	}

	// KARMAX's own servers still arrive: strict only drops everyone else's.
	withBrowser := spawnArgs(&Session{ID: "s", MCPConfig: `{"mcpServers":{}}`}, false, "")
	if !slices.Contains(withBrowser, "--strict-mcp-config") || !slices.Contains(withBrowser, "--mcp-config") {
		t.Errorf("args = %v, want both --strict-mcp-config and --mcp-config", withBrowser)
	}
	// --mcp-config is variadic and eats what follows it, so the boolean flag
	// must not come after it.
	if i, j := slices.Index(withBrowser, "--strict-mcp-config"), slices.Index(withBrowser, "--mcp-config"); i > j {
		t.Errorf("args = %v: --strict-mcp-config after --mcp-config would be read as a config", withBrowser)
	}
}

// A kind the operator opts in keeps the old behaviour: every server in their
// own config, for work that really uses them.
func TestAKindThatInheritsMCPGetsTheOperatorsServers(t *testing.T) {
	args := spawnArgs(&Session{ID: "s", InheritMCP: true}, false, "")
	if slices.Contains(args, "--strict-mcp-config") {
		t.Errorf("args = %v, want no --strict-mcp-config for a kind that inherits MCP", args)
	}
}

// The policy has to reach the process, not just the struct.
func TestTheKindsMCPScopeReachesTheSpawnedProcess(t *testing.T) {
	for _, tc := range []struct {
		inherit    bool
		wantStrict bool
	}{{false, true}, {true, false}} {
		bin, argsFile := writeArgRecordingClaude(t)
		sup := New(Config{
			Binary: bin, WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
			Policies: map[string]Policy{"k": {TurnTimeout: 5 * time.Second, InheritMCP: tc.inherit}},
		}, newMemStore(), NewBreaker(0.95, nil), testLog{t}, nil)
		t.Cleanup(sup.Shutdown)
		if _, err := sup.Send(context.Background(), "key", "k", "hi"); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(readArgs(t, argsFile), "--strict-mcp-config"); got != tc.wantStrict {
			t.Errorf("inherit_mcp=%v: --strict-mcp-config present = %v, want %v", tc.inherit, got, tc.wantStrict)
		}
	}
}
