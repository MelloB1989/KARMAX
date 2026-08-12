package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
)

type stubTool struct{ name, desc string }

func (s *stubTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{Name: s.name, Description: s.desc, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (s *stubTool) Execute(context.Context, map[string]any) (tools.ToolResult, error) {
	return tools.SuccessResult(nil), nil
}

func toolSet(names ...string) []tools.Tool {
	var out []tools.Tool
	for _, n := range names {
		out = append(out, &stubTool{name: n, desc: "Does " + n + ". More detail here."})
	}
	return out
}

func TestSplitHoldsCoreAndIndexesTheRest(t *testing.T) {
	a := &Agent{def: AgentDef{CoreTools: []string{"comms.send", "memory.retrieve"}}}
	held, indexed := a.splitToolSet(toolSet("comms.send", "memory.retrieve", "google_workspace", "wacli"))

	if len(held) != 2 {
		t.Fatalf("held = %v", manifestNames(held))
	}
	if len(indexed) != 2 {
		t.Fatalf("indexed = %v", manifestNames(indexed))
	}
	for _, n := range manifestNames(indexed) {
		if n == "comms.send" {
			t.Error("a core tool was indexed instead of held")
		}
	}
}

// The escape hatch: "*" keeps the old behaviour of shipping every schema.
func TestStarKeepsEveryToolInFull(t *testing.T) {
	a := &Agent{def: AgentDef{CoreTools: []string{"*"}}}
	held, indexed := a.splitToolSet(toolSet("a", "b", "c"))
	if len(held) != 3 || len(indexed) != 0 {
		t.Errorf("held=%d indexed=%d, want everything held", len(held), len(indexed))
	}
}

func TestEmptyCoreUsesTheDefault(t *testing.T) {
	a := &Agent{def: AgentDef{}}
	held, indexed := a.splitToolSet(toolSet("comms.send", "some.other.tool"))
	if len(held) != 1 || held[0].Manifest().Name != "comms.send" {
		t.Errorf("default core not applied: held=%v", manifestNames(held))
	}
	if len(indexed) != 1 {
		t.Errorf("indexed = %v", manifestNames(indexed))
	}
}

// Dotted and underscored names refer to the same tool.
func TestSplitMatchesCanonicalNames(t *testing.T) {
	a := &Agent{def: AgentDef{CoreTools: []string{"comms_send"}}}
	held, _ := a.splitToolSet(toolSet("comms.send"))
	if len(held) != 1 {
		t.Error("canonical core name did not match the dotted tool")
	}
}

func TestLendNamedResolvesFromTheFullSet(t *testing.T) {
	a := &Agent{allTools: toolSet("google_workspace", "wacli", "comms.send")}
	got := a.lendNamed([]string{"wacli", "nonexistent"})
	if len(got) != 1 || got[0].Manifest().Name != "wacli" {
		t.Errorf("lendNamed = %v", manifestNames(got))
	}
}

// The load request is read out of the turn's tool records — which only became
// possible once tool calls were observable at all.
func TestRequestedToolsReadsTheLoadCall(t *testing.T) {
	calls := []karmahelper.ToolCallRecord{
		{Name: "memory.retrieve"},
		{
			Name:   "tools.load",
			Input:  map[string]any{"names": []any{"google_workspace"}},
			Result: tools.SuccessResult(map[string]any{"loaded": []string{"google_workspace"}}),
		},
	}
	got := requestedTools(calls)
	if len(got) != 1 || got[0] != "google_workspace" {
		t.Errorf("requestedTools = %v", got)
	}
}

// If the result shape is not what we expect, fall back to what was asked for
// rather than silently loading nothing.
func TestRequestedToolsFallsBackToInput(t *testing.T) {
	calls := []karmahelper.ToolCallRecord{{
		Name:   "tools.load",
		Input:  map[string]any{"names": []any{"wacli"}},
		Result: tools.SuccessResult("unexpected shape"),
	}}
	if got := requestedTools(calls); len(got) != 1 || got[0] != "wacli" {
		t.Errorf("requestedTools = %v", got)
	}
}

func TestNoLoadCallMeansNoRequest(t *testing.T) {
	if got := requestedTools([]karmahelper.ToolCallRecord{{Name: "comms.send"}}); len(got) != 0 {
		t.Errorf("requestedTools = %v", got)
	}
}

// The whole point: the index must be dramatically smaller than the schemas.
func TestIndexIsMuchSmallerThanSchemas(t *testing.T) {
	var full int
	var ts []tools.Tool
	for i := 0; i < 60; i++ {
		st := &stubTool{
			name: "tool.number" + string(rune('a'+i%26)),
			desc: strings.Repeat("a long description with plenty of detail. ", 8),
		}
		ts = append(ts, st)
		m := st.Manifest()
		full += len(m.Name) + len(m.Description) + len(m.Parameters)
	}
	idx := len(tools.Index(manifestsOf(ts)))
	if idx >= full/3 {
		t.Errorf("index (%d chars) is not meaningfully smaller than schemas (%d chars)", idx, full)
	}
}
