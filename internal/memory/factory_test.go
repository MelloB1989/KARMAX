package memory

import "testing"

func TestPrimaryNamespaceMapsAcrossUnchanged(t *testing.T) {
	f := NewFactory(t.TempDir(), nil, nil)
	// GITLOOM_NAMESPACE=karmax with the agent's local namespace "nexus".
	f.gitloom = &GitLoomConfig{Namespace: "karmax", PrimaryLocal: "nexus"}

	// The operator said their memory is at "karmax". Redirecting the primary
	// agent to "karmax-nexus" points the daemon at an empty namespace beside
	// the one everything was migrated into — the whole memory silently absent.
	if got := f.namespaceFor("nexus"); got != "karmax" {
		t.Errorf("primary namespace = %q, want karmax", got)
	}
	// A second agent still gets its own, so it cannot write into the first's.
	if got := f.namespaceFor("research"); got != "karmax-research" {
		t.Errorf("secondary namespace = %q, want karmax-research", got)
	}
}

func TestNoGitLoomLeavesNamespacesAlone(t *testing.T) {
	f := NewFactory(t.TempDir(), nil, nil)
	if got := f.namespaceFor("nexus"); got != "nexus" {
		t.Errorf("namespace = %q, want nexus", got)
	}
}
