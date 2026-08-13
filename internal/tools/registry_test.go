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

type namedTestTool string

func (n namedTestTool) Manifest() ToolManifest {
	return ToolManifest{Name: string(n), Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (namedTestTool) Execute(context.Context, map[string]any) (ToolResult, error) {
	return SuccessResult("ok"), nil
}

func TestResolveForAgentExpandsPrefixGlobs(t *testing.T) {
	reg := NewRegistry()
	reg.Register(namedTestTool("whatsapp_send_message"))
	reg.Register(namedTestTool("whatsapp_list_chats"))
	reg.Register(namedTestTool("whatsapp.read"))
	reg.Register(namedTestTool("shell.exec"))

	resolved, unresolved := reg.ResolveForAgent([]string{"whatsapp_*", "shell.exec"})
	if len(unresolved) != 0 {
		t.Fatalf("expected everything to resolve, got unresolved %v", unresolved)
	}
	// whatsapp.read is registered under whatsapp_read too, so the glob picks it
	// up; the point is that it appears once, not twice.
	names := make([]string, 0, len(resolved))
	for _, r := range resolved {
		names = append(names, r.Manifest().Name)
	}
	if len(names) != 4 {
		t.Fatalf("expected 4 deduped tools, got %d: %v", len(names), names)
	}

	// Stable order, or the prompt cache is invalidated on every restart.
	for i := 1; i < len(resolved)-1; i++ {
		if resolved[i-1].Manifest().Name > resolved[i].Manifest().Name {
			t.Fatalf("glob matches must be sorted, got %v", names)
		}
	}

	if _, un := reg.ResolveForAgent([]string{"nothing_*"}); len(un) != 1 {
		t.Fatalf("a glob matching nothing must be reported unresolved, got %v", un)
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
