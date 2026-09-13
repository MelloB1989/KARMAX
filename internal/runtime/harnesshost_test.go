package runtime

import (
	"context"
	"testing"

	"github.com/MelloB1989/karmax/internal/browser"
)

// fakeBrowser stands in for *browser.Session: just enough to drive
// browserMCPConfig without a real Chromium.
type fakeBrowser struct {
	cfg string
	err error
}

func (f fakeBrowser) MCPConfigJSON(context.Context) (string, error) { return f.cfg, f.err }

// A chat (or agent) session gets the browser's MCP config when it is running.
func TestChatSessionGetsTheBrowserWhenItIsRunning(t *testing.T) {
	for _, kind := range []string{"chat", "agent"} {
		br := fakeBrowser{cfg: `{"mcpServers":{}}`}
		if got := browserMCPConfig(context.Background(), br, kind); got != br.cfg {
			t.Fatalf("%s: MCPConfig = %q, want %q", kind, got, br.cfg)
		}
	}
}

// The browser is usually closed. That is normal, not an error, and the turn
// proceeds with no browser tools at all.
func TestChatSessionGetsNoBrowserWhenItIsNotRunning(t *testing.T) {
	br := fakeBrowser{err: browser.ErrNotRunning}
	if got := browserMCPConfig(context.Background(), br, "chat"); got != "" {
		t.Fatalf("MCPConfig = %q, want empty when the browser is not running", got)
	}
}

// utility sessions are cheap one-shot judgments with no use for a browser,
// even when one happens to be open.
func TestUtilityKindNeverGetsTheBrowser(t *testing.T) {
	br := fakeBrowser{cfg: `{"mcpServers":{}}`}
	if got := browserMCPConfig(context.Background(), br, "utility"); got != "" {
		t.Fatalf("MCPConfig = %q, want empty for the utility kind", got)
	}
}
