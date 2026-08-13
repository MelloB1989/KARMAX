package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MelloB1989/karmax/internal/tools"
)

type capTool string

func (c capTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{Name: string(c), Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (capTool) Execute(context.Context, map[string]any) (tools.ToolResult, error) {
	return tools.SuccessResult("ok"), nil
}

func capabilitiesOutput(t *testing.T, tool *CapabilitiesTool) map[string]any {
	t.Helper()
	res, err := tool.Execute(context.Background(), map[string]any{"section": "tools"})
	if err != nil || res.IsError {
		t.Fatalf("execute: %v %v", err, res.Error)
	}
	out, ok := res.Output.(map[string]any)
	if !ok {
		t.Fatalf("unexpected output %T", res.Output)
	}
	return out
}

func names(v any) []string {
	s, _ := v.([]string)
	return s
}

func TestCapabilitiesReportsTheAgentsOwnLoadableSet(t *testing.T) {
	// The registry holds more than this agent was granted. Reporting the
	// registry as "loadable" told the agent it could fetch tools that
	// tools.load cannot bind, because binding resolves against the agent's set.
	reg := tools.NewRegistry()
	reg.Register(capTool("comms.send"))
	reg.Register(capTool("whatsapp.read"))
	reg.Register(capTool("linkedin.post"))

	out := capabilitiesOutput(t, &CapabilitiesTool{
		Registry: reg,
		Held:     []string{"comms.send"},
		Loadable: []string{"whatsapp.read"},
	})

	for _, n := range names(out["tools_loadable"]) {
		if n == "linkedin.post" {
			t.Error("a tool the agent was never granted must not be reported as loadable")
		}
	}
	if got := names(out["tools_held"]); len(got) != 1 || got[0] != "comms.send" {
		t.Errorf("tools_held = %v", got)
	}
}

func TestCapabilitiesNamesToolsPresentButNotGranted(t *testing.T) {
	// The real case: LinkedIn connected, linkedin.post registered, and the agent
	// told the operator KARMAX had no LinkedIn integration.
	reg := tools.NewRegistry()
	reg.Register(capTool("comms.send"))
	reg.Register(capTool("linkedin.post"))

	out := capabilitiesOutput(t, &CapabilitiesTool{
		Registry: reg,
		Held:     []string{"comms.send"},
	})

	found := false
	for _, n := range names(out["tools_on_this_instance_but_not_yours"]) {
		if n == "linkedin.post" {
			found = true
		}
	}
	if !found {
		t.Errorf("linkedin.post should be reported as ungranted, got %v", out["tools_on_this_instance_but_not_yours"])
	}
	if out["ungranted_note"] == nil {
		t.Error("the ungranted list needs the instruction that goes with it")
	}
}

func TestCapabilitiesSaysNothingAboutUngrantedWhenThereAreNone(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(capTool("comms.send"))

	out := capabilitiesOutput(t, &CapabilitiesTool{Registry: reg, Held: []string{"comms.send"}})
	if _, present := out["tools_on_this_instance_but_not_yours"]; present {
		t.Error("an agent granted everything should see no ungranted section")
	}
}
