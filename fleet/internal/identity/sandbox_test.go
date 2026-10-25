package identity

import "testing"

func TestASandboxKeyIsItsAgentsAndNeverAnAgentKey(t *testing.T) {
	k := SandboxKey("full", "agent-09")
	if len(k) != 64 || k == SandboxKey("full", "agent-10") || k == SandboxKey("other", "agent-09") || k != SandboxKey("full", "agent-09") {
		t.Fatalf("sandbox keys do not tell agents and tokens apart: %s", k)
	}
	if k == Key("full", "agent-09") {
		t.Fatal("a sandbox key is the same agent's relay key")
	}
	if SandboxKey("", "agent-09") != "" {
		t.Fatal("no full token makes a key anyway")
	}
}

func TestSandboxOfFindsOnlyAConfiguredSandbox(t *testing.T) {
	agents := []string{"agent-09", "agent-10"}
	if a, ok := SandboxOf("full", SandboxKey("full", "agent-10"), agents); !ok || a != "agent-10" {
		t.Errorf("SandboxOf = %q, %v", a, ok)
	}
	for _, bearer := range []string{SandboxKey("full", "agent-03"), Key("full", "agent-09"), SandboxKey("other", "agent-09"), "", "full"} {
		if a, ok := SandboxOf("full", bearer, agents); ok {
			t.Errorf("SandboxOf(%q) = %q", bearer, a)
		}
	}
	if _, ok := SandboxOf("", SandboxKey("", "agent-09"), agents); ok {
		t.Error("no full token, yet a sandbox")
	}
}
