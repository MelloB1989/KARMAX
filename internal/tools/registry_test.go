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
