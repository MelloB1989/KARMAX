package tools

import (
	"context"
	"encoding/json"
	"testing"
)

type registryTestTool struct{}

func (registryTestTool) Manifest() ToolManifest {
	return ToolManifest{
		Name:       "shell.exec",
		Parameters: json.RawMessage(`{"type":"object"}`),
	}
}

func (registryTestTool) Execute(context.Context, map[string]any) (ToolResult, error) {
	return SuccessResult("ok"), nil
}

func TestRegistrySupportsCanonicalToolNames(t *testing.T) {
	reg := NewRegistry()
	reg.Register(registryTestTool{})

	if _, ok := reg.Get("shell.exec"); !ok {
		t.Fatal("expected dotted tool name lookup to work")
	}
	if _, ok := reg.Get("shell_exec"); !ok {
		t.Fatal("expected canonical underscore tool name lookup to work")
	}

	if got := CanonicalName("memory.retrieve"); got != "memory_retrieve" {
		t.Fatalf("CanonicalName() = %q, want memory_retrieve", got)
	}

	if got := len(reg.List()); got != 1 {
		t.Fatalf("List() should dedupe dotted/canonical registrations, got %d", got)
	}
}

func TestResolveForAgentSkipsUnknownTools(t *testing.T) {
	reg := NewRegistry()
	reg.Register(registryTestTool{}) // registers shell.exec

	resolved, unresolved := reg.ResolveForAgent([]string{"shell.exec", "memory.ingest", "does.not.exist"})
	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved tool, got %d", len(resolved))
	}
	if len(unresolved) != 2 {
		t.Fatalf("expected 2 unresolved names, got %v", unresolved)
	}
}

func TestIsAgentScoped(t *testing.T) {
	scoped := []string{"memory.ingest", "memory.retrieve", "comms.escalate", "profile.update", "memory_ingest"}
	for _, name := range scoped {
		if !IsAgentScoped(name) {
			t.Errorf("expected %q to be agent-scoped", name)
		}
	}
	global := []string{"shell.exec", "claude_code.call", "does.not.exist"}
	for _, name := range global {
		if IsAgentScoped(name) {
			t.Errorf("expected %q NOT to be agent-scoped", name)
		}
	}
}
