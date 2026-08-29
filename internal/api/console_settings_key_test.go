package api

import "testing"

// The console saved the coding-agent token under one key and the sandbox read
// another, so an operator could save a token, see it stored with its last four
// digits shown back, and still have every sandbox fail for want of one.
//
// The runtime names the same string. If either side is renamed without the
// other, this fails rather than silently going quiet again.
func TestTheSandboxTokenKeyMatchesTheRuntime(t *testing.T) {
	const runtimeKey = "sandbox_agent_token"
	if sandboxTokenKey != runtimeKey {
		t.Fatalf("the console writes %q but the runtime reads %q — a saved token would never be found",
			sandboxTokenKey, runtimeKey)
	}
}
