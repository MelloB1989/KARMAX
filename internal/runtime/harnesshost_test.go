package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/MelloB1989/karmax/internal/browser"
	"go.uber.org/zap"
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

// Chat and agent sessions get the skills directory, same as they get the
// browser.
func TestHarnessPluginDirForBrowserKinds(t *testing.T) {
	for _, kind := range []string{"chat", "agent"} {
		if got := harnessPluginDir(kind, "/skills"); got != "/skills" {
			t.Fatalf("%s: PluginDir = %q, want /skills", kind, got)
		}
	}
}

// utility gets no skills either, for the same reason it gets no browser.
func TestHarnessPluginDirForOtherKinds(t *testing.T) {
	if got := harnessPluginDir("utility", "/skills"); got != "" {
		t.Fatalf("PluginDir = %q, want empty for the utility kind", got)
	}
}

// A Materialise failure must not stop chat and agent sessions from working —
// they run without --plugin-dir, the same as when the browser is closed.
func TestMaterialiseFailureLeavesPluginDirEmpty(t *testing.T) {
	dataDir := t.TempDir()
	// A file where Materialise wants a directory forces os.MkdirAll to fail,
	// standing in for an unwritable disk.
	blocked := filepath.Join(dataDir, "blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := materialiseSkills(blocked, zap.NewNop())
	if dir != "" {
		t.Fatalf("materialiseSkills = %q, want empty on failure", dir)
	}
	if got := harnessPluginDir("chat", dir); got != "" {
		t.Fatalf("PluginDir = %q, want empty when nothing was materialised", got)
	}
}

// fakeSupervisor stands in for *harness.Supervisor: just enough to drive
// recycleIdleBrowserSessions without a real process.
type fakeSupervisor struct {
	live    []string
	busyKey map[string]bool
	closed  []string
}

func (f *fakeSupervisor) Live() []string       { return f.live }
func (f *fakeSupervisor) Busy(key string) bool { return f.busyKey[key] }
func (f *fakeSupervisor) Close(key string)     { f.closed = append(f.closed, key) }

// The browser starting is the trigger: a chat session's next turn needs the
// tools that just became available, and its baked-in --mcp-config has none.
func TestBrowserStartClosesIdleChatSessions(t *testing.T) {
	sup := &fakeSupervisor{live: []string{"chat:1"}, busyKey: map[string]bool{}}
	recycleIdleBrowserSessions(sup, map[string]string{"chat:1": "chat"}, browserKinds)
	if len(sup.closed) != 1 || sup.closed[0] != "chat:1" {
		t.Fatalf("closed = %v, want [chat:1]", sup.closed)
	}
}

// The browser stopping is the same trigger in the other direction: a warm
// session's baked-in --mcp-config now points at a browser that is gone.
func TestBrowserStopClosesIdleChatSessions(t *testing.T) {
	sup := &fakeSupervisor{live: []string{"agent:foo"}, busyKey: map[string]bool{}}
	recycleIdleBrowserSessions(sup, map[string]string{"agent:foo": "agent"}, browserKinds)
	if len(sup.closed) != 1 || sup.closed[0] != "agent:foo" {
		t.Fatalf("closed = %v, want [agent:foo]", sup.closed)
	}
}

// Closing a busy session would kill a running answer in front of the
// operator — the constraint that matters most.
func TestABusySessionIsLeftAlone(t *testing.T) {
	sup := &fakeSupervisor{live: []string{"chat:1"}, busyKey: map[string]bool{"chat:1": true}}
	recycleIdleBrowserSessions(sup, map[string]string{"chat:1": "chat"}, browserKinds)
	if len(sup.closed) != 0 {
		t.Fatalf("closed = %v, want none — the session was busy", sup.closed)
	}
}

// utility never gets a browser, so a browser event is none of its business.
func TestUtilitySessionsAreNotRecycled(t *testing.T) {
	sup := &fakeSupervisor{live: []string{"summary:1"}, busyKey: map[string]bool{}}
	recycleIdleBrowserSessions(sup, map[string]string{"summary:1": "utility"}, browserKinds)
	if len(sup.closed) != 0 {
		t.Fatalf("closed = %v, want none — utility never gets the browser", sup.closed)
	}
}
