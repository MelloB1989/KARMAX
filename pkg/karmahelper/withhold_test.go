package karmahelper

import (
	"context"
	"testing"

	"github.com/MelloB1989/karmax/internal/tools"
)

type namedTool struct{ name string }

func (n namedTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{Name: n.name, Description: "x"}
}
func (n namedTool) Execute(context.Context, map[string]any) (tools.ToolResult, error) {
	return tools.ToolResult{}, nil
}

// A pass that must not speak is handed no way to speak. hot-sync was told in
// capitals not to send and sent anyway; the tool list is the only thing the
// model cannot argue with.
func TestWithheldToolsAreNotOfferedToTheModel(t *testing.T) {
	base := []tools.Tool{
		namedTool{"comms.send"}, namedTool{"memory.ingest"}, namedTool{"whatsapp.read"},
	}
	got := turnToolSet(base, nil, map[string]bool{"comms_send": true})

	for _, tool := range got {
		if tools.CanonicalName(tool.Manifest().Name) == "comms_send" {
			t.Fatal("comms.send was withheld and must not be in the turn's tools")
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d tools, want the 2 that were not withheld", len(got))
	}
}

func TestNothingWithheldKeepsEveryTool(t *testing.T) {
	base := []tools.Tool{namedTool{"comms.send"}, namedTool{"memory.ingest"}}
	extra := []tools.Tool{namedTool{"wacli"}}
	if got := turnToolSet(base, extra, nil); len(got) != 3 {
		t.Fatalf("got %d tools, want 3", len(got))
	}
}

// Lending a tool must not smuggle past a withhold.
func TestALentToolCanAlsoBeWithheld(t *testing.T) {
	base := []tools.Tool{namedTool{"memory.ingest"}}
	extra := []tools.Tool{namedTool{"comms.send"}}
	got := turnToolSet(base, extra, map[string]bool{"comms_send": true})
	if len(got) != 1 {
		t.Fatalf("got %d tools, want 1 — a lent tool is still subject to the withhold", len(got))
	}
}
