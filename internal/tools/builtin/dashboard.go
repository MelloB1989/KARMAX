package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/dashboards"
	"github.com/MelloB1989/karmax/internal/tools"
)

// Giving an agent a screen instead of just a chat bubble.
//
// A dashboard is one HTML file a client renders standalone, plus JSON data
// files it can re-read without asking the agent again — the difference
// between "here is a paragraph describing your numbers" and "here is a page
// that still shows them tomorrow". 'components' exists because an agent that
// never sees the installed kit reinvents a card-and-chart vocabulary from
// scratch on every dashboard, and no two of them end up looking related.

// DashboardTool is the whole surface: one tool, six actions, matching the
// single name a browser-scoped harness call and the scoped-token allowlist
// both key off (see browserScopedTools in internal/api/server.go).
type DashboardTool struct {
	// AgentID is bound per-agent (see bindAgentTools) and recorded on
	// whatever a 'save' or 'set_data' call touches, purely as attribution —
	// nothing here is refused for lacking one, unlike scheduler.add, since a
	// bare API client saving a dashboard is a normal case, not a bug.
	AgentID string
}

func (t *DashboardTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "dashboard",
		Description: "Build an interactive dashboard: a standalone HTML page plus JSON data files, optionally kept current on a schedule. " +
			"Call 'components' first and read what it returns before writing any HTML. " +
			"'save' creates or updates one (html is required the first time); 'set_data' updates a single data file without touching the HTML; " +
			"'get' returns one dashboard's metadata and HTML; 'list' lists all of them; 'delete' removes one. " +
			"Metadata includes 'pinned' and 'archived' — archived means the operator put it away; this tool cannot change either.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["components", "save", "set_data", "get", "list", "delete"]},
				"id": {"type": "string", "description": "Dashboard id, e.g. \"sales-overview\". For 'save', defaults to a slug of \"title\". Required for 'set_data', 'get', 'delete'."},
				"title": {"type": "string", "description": "Required for 'save'. Up to 120 characters."},
				"description": {"type": "string", "description": "For 'save'. Up to 500 characters. Omit to leave an existing one unchanged."},
				"html": {"type": "string", "description": "For 'save'. The full page, up to 512 KB. Required the first time this dashboard is saved."},
				"data": {"type": "object", "description": "For 'save'. Maps data file name -> any JSON value to write. Existing data files not named here are kept as they are.", "additionalProperties": true},
				"name": {"type": "string", "description": "Data file name, for 'set_data'."},
				"value": {"description": "The JSON value to write, for 'set_data'."},
				"live": {"type": "array", "items": {"type": "string"}, "description": "For 'save'. Names of data files a client should treat as pushed live rather than only refreshed on a schedule. Omit to leave unchanged."},
				"refresh": {
					"description": "For 'save'. {\"every\": \"1h\"} or {\"cron\": \"0 0 9 * * *\"}, plus a required \"brief\" describing what to refresh and how. Omit to leave the existing schedule alone; pass null to remove it.",
					"properties": {
						"every": {"type": "string"},
						"cron": {"type": "string"},
						"brief": {"type": "string"}
					}
				}
			},
			"required": ["action"]
		}`),
	}
}

func (t *DashboardTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	action := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", input["action"])))
	switch action {
	case "components":
		return tools.SuccessResult(map[string]any{"reference": dashboards.ComponentReference()}), nil

	case "save":
		in, err := parseDashboardSaveInput(input, t.AgentID)
		if err != nil {
			return tools.ErrorResult(err), nil
		}
		meta, err := dashboards.Save(in)
		if err != nil {
			return tools.ErrorResult(err), nil
		}
		return tools.SuccessResult(map[string]any{
			"id": meta.ID, "saved": true, "htmlVersion": meta.HTMLVersion, "dataVersion": meta.DataVersion,
		}), nil

	case "set_data":
		id, _ := input["id"].(string)
		name, _ := input["name"].(string)
		value, present := input["value"]
		if !present {
			return tools.ErrorResult(fmt.Errorf("value is required")), nil
		}
		meta, err := dashboards.SetData(id, name, value, t.AgentID)
		if err != nil {
			return tools.ErrorResult(err), nil
		}
		return tools.SuccessResult(map[string]any{"ok": true, "dataVersion": meta.DataVersion}), nil

	case "get":
		id, _ := input["id"].(string)
		meta, html, err := dashboards.Get(id)
		if err != nil {
			return tools.ErrorResult(err), nil
		}
		return tools.SuccessResult(map[string]any{"dashboard": meta, "html": html}), nil

	case "list":
		metas, err := dashboards.List()
		if err != nil {
			return tools.ErrorResult(err), nil
		}
		if metas == nil {
			metas = []dashboards.Meta{}
		}
		return tools.SuccessResult(map[string]any{"dashboards": metas}), nil

	case "delete":
		id, _ := input["id"].(string)
		if err := dashboards.Delete(id); err != nil {
			return tools.ErrorResult(err), nil
		}
		return tools.SuccessResult(map[string]any{"deleted": true}), nil
	}
	return tools.ErrorResult(fmt.Errorf("unknown action %q (use components, save, set_data, get, list, delete)", action)), nil
}

// parseDashboardSaveInput reads a 'save' call's map[string]any into a
// dashboards.SaveInput, tracking which optional fields were present at all —
// a JSON key set to null and a key left out arrive here identically as far
// as Go's map is concerned unless it is checked with the ", ok" form, and
// "leave unchanged" vs. "clear it" is exactly the distinction that check
// carries.
func parseDashboardSaveInput(input map[string]any, agentID string) (dashboards.SaveInput, error) {
	in := dashboards.SaveInput{Agent: agentID}
	if v, ok := input["id"].(string); ok {
		in.ID = v
	}
	in.Title, _ = input["title"].(string)

	if v, present := input["description"]; present {
		s, ok := v.(string)
		if !ok {
			return in, fmt.Errorf("description must be a string")
		}
		in.Description, in.DescriptionSet = s, true
	}
	if v, present := input["html"]; present {
		s, ok := v.(string)
		if !ok {
			return in, fmt.Errorf("html must be a string")
		}
		in.HTML, in.HTMLSet = s, true
	}
	if v, present := input["data"]; present {
		m, ok := v.(map[string]any)
		if !ok {
			return in, fmt.Errorf("data must be an object mapping name -> value")
		}
		in.Data = m
	}
	if v, present := input["live"]; present {
		list, err := dashboardStringList(v)
		if err != nil {
			return in, fmt.Errorf("live: %w", err)
		}
		in.Live, in.LiveSet = list, true
	}
	if v, present := input["refresh"]; present {
		in.RefreshSet = true
		if v != nil {
			m, ok := v.(map[string]any)
			if !ok {
				return in, fmt.Errorf("refresh must be an object or null")
			}
			r := &dashboards.Refresh{}
			r.Every, _ = m["every"].(string)
			r.Cron, _ = m["cron"].(string)
			r.Brief, _ = m["brief"].(string)
			in.Refresh = r
		}
	}
	return in, nil
}

func dashboardStringList(v any) ([]string, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("must be a list of strings")
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("must be a list of strings")
		}
		out = append(out, s)
	}
	return out, nil
}
