package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

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
//
// browserKinds is an ALLOWLIST of {"chat","agent"}, not a denylist of
// {"utility"} — so this alone would pass identically against a hardcoded
// denylist implementation, proving nothing about the actual rule. The kind
// below is not on the branch that added "utility" and never will be one
// KARMAX itself defines; a real allowlist still refuses it.
func TestUtilityKindNeverGetsTheBrowser(t *testing.T) {
	br := fakeBrowser{cfg: `{"mcpServers":{}}`}
	for _, kind := range []string{"utility", "some-kind-nobody-registered"} {
		if got := browserMCPConfig(context.Background(), br, kind); got != "" {
			t.Fatalf("%s: MCPConfig = %q, want empty for a kind not on the allowlist", kind, got)
		}
	}
}

// fakeSupervisor stands in for *harness.Supervisor: just enough to drive
// recycleIdleBrowserSessions without a real process. recycleIdleBrowserSessions
// now calls CloseIfIdle from its own goroutine per key, so closed and
// busyKey are guarded — a plain slice/map append from concurrent goroutines
// would be a real data race, not just a theoretical one.
type fakeSupervisor struct {
	live []string
	// closeDelay simulates a slow teardown (a real Close() waiting out a
	// stubborn process's SIGKILL fallback), so a test can tell "closed one
	// at a time" from "closed in parallel" by wall-clock time.
	closeDelay time.Duration

	mu      sync.Mutex
	busyKey map[string]bool
	closed  []string
}

func (f *fakeSupervisor) Live() []string { return f.live }

func (f *fakeSupervisor) CloseIfIdle(key string) bool {
	if f.closeDelay > 0 {
		time.Sleep(f.closeDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busyKey[key] {
		return false
	}
	f.closed = append(f.closed, key)
	return true
}

// closedKeys is a synchronized snapshot of what got closed, for assertions
// made after recycleIdleBrowserSessions has returned (it waits on its own
// goroutines, so by then there is nothing left to race against — this is
// just so `go vet`/the race detector see every access going through the same
// lock).
func (f *fakeSupervisor) closedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closed...)
}

// The browser starting is the trigger: a chat session's next turn needs the
// tools that just became available, and its baked-in --mcp-config has none.
func TestBrowserStartClosesIdleChatSessions(t *testing.T) {
	sup := &fakeSupervisor{live: []string{"chat:1"}, busyKey: map[string]bool{}}
	recycleIdleBrowserSessions(sup, map[string]string{"chat:1": "chat"}, browserKinds)
	if got := sup.closedKeys(); len(got) != 1 || got[0] != "chat:1" {
		t.Fatalf("closed = %v, want [chat:1]", got)
	}
}

// The browser stopping is the same trigger in the other direction: a warm
// session's baked-in --mcp-config now points at a browser that is gone.
func TestBrowserStopClosesIdleChatSessions(t *testing.T) {
	sup := &fakeSupervisor{live: []string{"agent:foo"}, busyKey: map[string]bool{}}
	recycleIdleBrowserSessions(sup, map[string]string{"agent:foo": "agent"}, browserKinds)
	if got := sup.closedKeys(); len(got) != 1 || got[0] != "agent:foo" {
		t.Fatalf("closed = %v, want [agent:foo]", got)
	}
}

// Closing a busy session would kill a running answer in front of the
// operator — the constraint that matters most.
func TestABusySessionIsLeftAlone(t *testing.T) {
	sup := &fakeSupervisor{live: []string{"chat:1"}, busyKey: map[string]bool{"chat:1": true}}
	recycleIdleBrowserSessions(sup, map[string]string{"chat:1": "chat"}, browserKinds)
	if got := sup.closedKeys(); len(got) != 0 {
		t.Fatalf("closed = %v, want none — the session was busy", got)
	}
}

// utility never gets a browser, so a browser event is none of its business.
func TestUtilitySessionsAreNotRecycled(t *testing.T) {
	sup := &fakeSupervisor{live: []string{"summary:1"}, busyKey: map[string]bool{}}
	recycleIdleBrowserSessions(sup, map[string]string{"summary:1": "utility"}, browserKinds)
	if got := sup.closedKeys(); len(got) != 0 {
		t.Fatalf("closed = %v, want none — utility never gets the browser", got)
	}
}

// A stubborn process can take up to 3s to give up its SIGKILL fallback.
// With several sessions to recycle, closing them one at a time would turn a
// single browser toggle into a multi-second stall; recycleIdleBrowserSessions
// must close them concurrently instead, so the whole pass costs about as
// much as the single slowest close, not their sum.
func TestRecycleIdleBrowserSessionsClosesConcurrently(t *testing.T) {
	const n = 5
	const delay = 80 * time.Millisecond

	live := make([]string, n)
	kindOf := make(map[string]string, n)
	for i := range live {
		live[i] = "chat:" + string(rune('a'+i))
		kindOf[live[i]] = "chat"
	}
	sup := &fakeSupervisor{live: live, busyKey: map[string]bool{}, closeDelay: delay}

	start := time.Now()
	recycleIdleBrowserSessions(sup, kindOf, browserKinds)
	elapsed := time.Since(start)

	if got := sup.closedKeys(); len(got) != n {
		t.Fatalf("closed %d sessions, want %d: %v", len(got), n, got)
	}
	// A sequential pass would take roughly n*delay (400ms here); a
	// concurrent one costs about one delay however many sessions there are.
	// The bound is generous — well under 2*delay — so this only fails if the
	// sessions were closed one at a time, not from ordinary scheduling noise.
	if elapsed > 2*delay {
		t.Fatalf("recycling %d sessions took %v at %v each; want roughly one delay's worth, not %d", n, elapsed, delay, n)
	}
}

// countingBrowser stands in for the real browser session, counting how many
// times its own MCPConfigJSON — the real loopback probe — actually runs, so
// a test can tell "cached" from "probed every time" apart.
type countingBrowser struct {
	mu    sync.Mutex
	calls int
	cfg   string
	err   error
}

func (c *countingBrowser) MCPConfigJSON(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.cfg, c.err
}

func (c *countingBrowser) set(cfg string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg, c.err = cfg, err
}

func (c *countingBrowser) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// A warm session's --mcp-config is fixed at spawn, so recomputing it on
// every turn against an already-running session is pure waste: the probe
// must happen once, not once per turn.
func TestBrowserMCPCacheProbesOnceForRepeatedWarmTurns(t *testing.T) {
	br := &countingBrowser{cfg: `{"mcpServers":{}}`}
	cache := newBrowserMCPCache(br)
	for i := 0; i < 5; i++ {
		if got := browserMCPConfig(context.Background(), cache, "chat"); got != br.cfg {
			t.Fatalf("turn %d: MCPConfig = %q, want %q", i, got, br.cfg)
		}
	}
	if got := br.count(); got != 1 {
		t.Fatalf("underlying probes = %d, want 1 — a warm session must not reprobe per turn", got)
	}
}

// The browser being closed is the normal state, not a miss to keep
// retrying — it caches exactly like a running one, with one probe however
// many turns ask while it stays that way.
func TestBrowserMCPCacheCachesTheBrowserBeingClosed(t *testing.T) {
	br := &countingBrowser{err: browser.ErrNotRunning}
	cache := newBrowserMCPCache(br)
	for i := 0; i < 3; i++ {
		if got := browserMCPConfig(context.Background(), cache, "chat"); got != "" {
			t.Fatalf("turn %d: MCPConfig = %q, want empty while the browser is closed", i, got)
		}
	}
	if got := br.count(); got != 1 {
		t.Fatalf("underlying probes = %d, want 1 even while the browser stays closed", got)
	}
}

// The cache must not survive the state it was cached for: once the browser
// actually starts or stops, the next turn needs the current answer, not the
// one from before the transition.
func TestBrowserMCPCacheRefreshesOnlyAfterInvalidate(t *testing.T) {
	br := &countingBrowser{cfg: `{"mcpServers":{"a":{}}}`}
	cache := newBrowserMCPCache(br)

	if got := browserMCPConfig(context.Background(), cache, "chat"); got != br.cfg {
		t.Fatalf("initial call = %q, want %q", got, br.cfg)
	}

	// The underlying browser's answer changes, but nothing has told the
	// cache to look again yet.
	br.set(`{"mcpServers":{"b":{}}}`, nil)
	if got := browserMCPConfig(context.Background(), cache, "chat"); got != `{"mcpServers":{"a":{}}}` {
		t.Fatalf("pre-invalidate call = %q, want the stale cached value", got)
	}

	cache.invalidate()
	if got := browserMCPConfig(context.Background(), cache, "chat"); got != `{"mcpServers":{"b":{}}}` {
		t.Fatalf("post-invalidate call = %q, want the new config", got)
	}
	if got := br.count(); got != 2 {
		t.Fatalf("underlying probes = %d, want exactly 2 (initial miss, then after invalidate)", got)
	}
}

// Turns run concurrently with the browser's own start/stop signal, which
// invalidates the cache from a different goroutine — run with `go test
// -race` for this to mean anything.
func TestBrowserMCPCacheIsSafeUnderConcurrentTurnsAndInvalidation(t *testing.T) {
	br := &countingBrowser{cfg: `{"mcpServers":{}}`}
	cache := newBrowserMCPCache(br)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			browserMCPConfig(context.Background(), cache, "chat")
		}()
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.invalidate()
		}()
	}
	wg.Wait()
}

// blockingBrowser blocks its first MCPConfigJSON call until released, so a
// test can land a concurrent invalidate() precisely while that first probe
// is still in flight — the exact window F2's generation guard exists to
// close. Every call after the first returns immediately, so the same fake
// can also confirm what happens on the next, unblocked call.
type blockingBrowser struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
	cfg     string
	err     error
}

func newBlockingBrowser(cfg string) *blockingBrowser {
	return &blockingBrowser{entered: make(chan struct{}), release: make(chan struct{}), cfg: cfg}
}

func (b *blockingBrowser) MCPConfigJSON(context.Context) (string, error) {
	b.mu.Lock()
	b.calls++
	first := b.calls == 1
	b.mu.Unlock()
	if first {
		close(b.entered)
		<-b.release
	}
	return b.cfg, b.err
}

func (b *blockingBrowser) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// F2, reproduced deterministically: invalidate() fires while a refresh's own
// probe is still in flight (unlocked, since it is a loopback network call
// and not a memory operation), and that probe is then allowed to complete
// anyway. Before the generation guard, refresh's unconditional write at the
// end unset what invalidate had just announced — valid ended true, pinned
// to the answer from before whatever transition invalidate was signalling,
// and nothing short of some later, unrelated toggle would ever cause a
// reprobe. Every chat and agent turn in between would spawn with the wrong
// --mcp-config.
func TestBrowserMCPCacheDiscardsAProbeInvalidatedWhileInFlight(t *testing.T) {
	br := newBlockingBrowser(`{"mcpServers":{"old":{}}}`)
	cache := newBrowserMCPCache(br)

	done := make(chan struct{})
	var got string
	var gotErr error
	go func() {
		defer close(done)
		got, gotErr = cache.MCPConfigJSON(context.Background())
	}()

	<-br.entered       // the probe is in flight, unlocked
	cache.invalidate() // announces: whatever this probe returns is already stale
	close(br.release)  // let it finish anyway
	<-done

	if gotErr != nil || got != `{"mcpServers":{"old":{}}}` {
		t.Fatalf("the in-flight caller got %q, %v; it should still see its own probe's real answer", got, gotErr)
	}

	cache.mu.Lock()
	valid := cache.valid
	cache.mu.Unlock()
	if valid {
		t.Fatal("the cache ended valid=true, pinned to an answer from before the invalidate that raced it — " +
			"every turn until some unrelated later toggle would spawn with this stale --mcp-config")
	}

	// The next caller must trigger a real reprobe, not inherit a cached hit
	// left over from the raced write.
	if got := browserMCPConfig(context.Background(), cache, "chat"); got != br.cfg {
		t.Fatalf("post-race call = %q, want %q", got, br.cfg)
	}
	if n := br.count(); n != 2 {
		t.Fatalf("underlying probes = %d, want 2 (the raced one, then a real reprobe) — "+
			"a lower count means the stale write was still cached despite the race", n)
	}
}
