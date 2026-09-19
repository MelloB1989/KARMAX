package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MelloB1989/karmax/internal/dashboards"
	"github.com/MelloB1989/karmax/internal/tools"
)

func isolateDashboards(t *testing.T) {
	t.Helper()
	t.Setenv("KARMAX_DATA_DIR", t.TempDir())
	t.Setenv("KARMAX_RECIPES_DIR", t.TempDir())
}

// callDashboard decodes body the same way handleCallTool decodes an HTTP
// request — through encoding/json into map[string]any — so a JSON null
// arrives as a present key mapped to nil, exactly as it would over the wire,
// rather than as whatever a hand-built Go map happens to contain.
func callDashboard(t *testing.T, tool *DashboardTool, body string) tools.ToolResult {
	t.Helper()
	var input map[string]any
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		t.Fatalf("bad test JSON: %v", err)
	}
	res, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func TestDashboardToolComponentsFallsBackWithNoKitInstalled(t *testing.T) {
	isolateDashboards(t)
	res := callDashboard(t, &DashboardTool{}, `{"action":"components"}`)
	if res.IsError {
		t.Fatalf("components errored: %s", res.Error)
	}
	out := res.Output.(map[string]any)
	if out["reference"] == "" {
		t.Fatal("reference should be the fallback text, not empty")
	}
}

func TestDashboardToolSaveRequiresHTMLOnCreate(t *testing.T) {
	isolateDashboards(t)
	res := callDashboard(t, &DashboardTool{}, `{"action":"save","title":"No HTML Yet"}`)
	if !res.IsError {
		t.Fatal("save with no html on a new dashboard should fail")
	}
}

func TestDashboardToolSaveRecordsAgentID(t *testing.T) {
	isolateDashboards(t)
	res := callDashboard(t, &DashboardTool{AgentID: "agent-7"}, `{"action":"save","title":"Ops","html":"<p></p>"}`)
	if res.IsError {
		t.Fatalf("save errored: %s", res.Error)
	}
	out := res.Output.(map[string]any)
	id := out["id"].(string)
	meta, _, err := dashboards.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if meta.Agent != "agent-7" {
		t.Fatalf("meta.Agent = %q, want %q", meta.Agent, "agent-7")
	}
}

func TestDashboardToolSaveSetDataGetListDelete(t *testing.T) {
	isolateDashboards(t)
	tool := &DashboardTool{AgentID: "a1"}

	res := callDashboard(t, tool, `{"action":"save","id":"ops","title":"Ops","html":"<p>hi</p>","data":{"count":1}}`)
	if res.IsError {
		t.Fatalf("save: %s", res.Error)
	}
	out := res.Output.(map[string]any)
	if out["id"] != "ops" || out["saved"] != true {
		t.Fatalf("save output = %+v", out)
	}
	if int(out["htmlVersion"].(int)) != 1 {
		t.Fatalf("htmlVersion = %v, want 1", out["htmlVersion"])
	}

	res = callDashboard(t, tool, `{"action":"set_data","id":"ops","name":"count","value":2}`)
	if res.IsError {
		t.Fatalf("set_data: %s", res.Error)
	}
	sdOut := res.Output.(map[string]any)
	if sdOut["ok"] != true || sdOut["dataVersion"] != 2 {
		t.Fatalf("set_data output = %+v", sdOut)
	}

	res = callDashboard(t, tool, `{"action":"get","id":"ops"}`)
	if res.IsError {
		t.Fatalf("get: %s", res.Error)
	}
	getOut := res.Output.(map[string]any)
	if getOut["html"] != "<p>hi</p>" {
		t.Fatalf("get html = %v", getOut["html"])
	}

	res = callDashboard(t, tool, `{"action":"list"}`)
	if res.IsError {
		t.Fatalf("list: %s", res.Error)
	}
	list := res.Output.(map[string]any)["dashboards"].([]dashboards.Meta)
	if len(list) != 1 || list[0].ID != "ops" {
		t.Fatalf("list = %+v", list)
	}

	res = callDashboard(t, tool, `{"action":"delete","id":"ops"}`)
	if res.IsError {
		t.Fatalf("delete: %s", res.Error)
	}
	if res.Output.(map[string]any)["deleted"] != true {
		t.Fatalf("delete output = %+v", res.Output)
	}

	res = callDashboard(t, tool, `{"action":"get","id":"ops"}`)
	if !res.IsError {
		t.Fatal("get after delete should fail")
	}
}

func TestDashboardToolUnknownActionIsAnError(t *testing.T) {
	isolateDashboards(t)
	res := callDashboard(t, &DashboardTool{}, `{"action":"frobnicate"}`)
	if !res.IsError {
		t.Fatal("an unrecognised action should fail")
	}
}

// A JSON-null "refresh" must remove an existing schedule, and an omitted
// "refresh" key must leave it alone — the whole reason parseDashboardSaveInput
// checks presence with ", ok" instead of just reading the value.
func TestDashboardToolRefreshTriState(t *testing.T) {
	isolateDashboards(t)
	tool := &DashboardTool{}

	res := callDashboard(t, tool, `{"action":"save","id":"d","title":"D","html":"<p></p>","refresh":{"every":"1h","brief":"keep it current"}}`)
	if res.IsError {
		t.Fatalf("save with refresh: %s", res.Error)
	}
	meta, _, err := dashboards.Get("d")
	if err != nil || meta.Refresh == nil || meta.Refresh.Every != "1h" {
		t.Fatalf("meta after save = %+v, err=%v", meta, err)
	}

	// Omitted: unchanged.
	res = callDashboard(t, tool, `{"action":"save","id":"d","title":"D renamed"}`)
	if res.IsError {
		t.Fatalf("save omitting refresh: %s", res.Error)
	}
	meta, _, _ = dashboards.Get("d")
	if meta.Refresh == nil {
		t.Fatal("refresh should survive a save that never mentions it")
	}

	// Explicit null: removed.
	res = callDashboard(t, tool, `{"action":"save","id":"d","title":"D renamed","refresh":null}`)
	if res.IsError {
		t.Fatalf("save with refresh:null: %s", res.Error)
	}
	meta, _, _ = dashboards.Get("d")
	if meta.Refresh != nil {
		t.Fatalf("refresh should have been removed, got %+v", meta.Refresh)
	}
}

func TestDashboardToolManifestName(t *testing.T) {
	m := (&DashboardTool{}).Manifest()
	if m.Name != "dashboard" {
		t.Fatalf("Manifest().Name = %q, want %q", m.Name, "dashboard")
	}
}
