package main

import (
	"errors"
	"testing"
	"time"
)

// TestBrowserRouteUsesTheEngineWhenReachable is the decision start/open/
// status/stop all now make, closing the split BUG 2 was: an engine
// answering /api/ping must be driven through its own tool API, never a
// direct *browser.Session this CLI process would launch or drive on its
// own — a second, different browser the engine's own capture never sees.
// reachable and callTool are injected so this never touches a real
// engine, a real browser, or the network.
func TestBrowserRouteUsesTheEngineWhenReachable(t *testing.T) {
	var toolCalled, directCalled bool
	route := browserRoute{
		reachable: func() bool { return true },
		callTool: func(input map[string]any, timeout time.Duration) (map[string]any, error) {
			toolCalled = true
			if input["action"] != "open" {
				t.Errorf("callTool action = %v, want %q", input["action"], "open")
			}
			return map[string]any{"opened": "https://example.com"}, nil
		},
	}
	direct := func() (map[string]any, error) {
		directCalled = true
		return map[string]any{"opened": "a direct Session should not have run"}, nil
	}

	out, err := route.run(map[string]any{"action": "open", "url": "https://example.com"}, 5*time.Second, direct)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !toolCalled {
		t.Error("engine reachable: callTool was never called")
	}
	if directCalled {
		t.Error("engine reachable: direct ran anyway — should have gone through the engine's own browser")
	}
	if out["opened"] != "https://example.com" {
		t.Errorf("result = %v, want the tool's own result", out)
	}
}

// TestBrowserRouteFallsBackToDirectWhenNoEngineIsReachable is the other
// half: a standalone CLI with no engine running must still work, driving
// its own *browser.Session rather than failing outright — that fallback
// is what keeps `karmax browser start` usable with no daemon up.
func TestBrowserRouteFallsBackToDirectWhenNoEngineIsReachable(t *testing.T) {
	var toolCalled, directCalled bool
	route := browserRoute{
		reachable: func() bool { return false },
		callTool: func(input map[string]any, timeout time.Duration) (map[string]any, error) {
			toolCalled = true
			return nil, errors.New("callTool should never run when no engine is reachable")
		},
	}
	direct := func() (map[string]any, error) {
		directCalled = true
		return map[string]any{"running": true}, nil
	}

	out, err := route.run(map[string]any{"action": "start"}, 5*time.Second, direct)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if toolCalled {
		t.Error("no engine reachable: callTool ran anyway")
	}
	if !directCalled {
		t.Error("no engine reachable: direct was never called")
	}
	if out["running"] != true {
		t.Errorf("result = %v, want direct's own result", out)
	}
}

// TestEngineReachableIsFalseWithNothingListening sanity-checks the real
// reachability probe (not just the fakes above): pointed at a port
// nothing answers on, it must say false — and quickly, on its own short
// timeout, not the API client's default 20s.
func TestEngineReachableIsFalseWithNothingListening(t *testing.T) {
	t.Setenv("KARMAX_API_URL", "http://127.0.0.1:1")
	start := time.Now()
	if engineReachable() {
		t.Error("engineReachable() = true with nothing listening on that port")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("engineReachable() took %s — want it bounded by its own short timeout", elapsed)
	}
}
