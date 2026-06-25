package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
)

// AppPushTool sends a push notification to the KARMAX phone app via Expo.
type AppPushTool struct {
	Store *store.Store
}

func (t *AppPushTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "app.push",
		Description: "Send a push notification to Nikhil's KARMAX phone app (Expo push). Use for proactive briefings, reminders, and alerts that should surface in the app. Returns gracefully if no device is registered.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string", "description": "Notification title."},
				"body": {"type": "string", "description": "Notification body."},
				"data": {"type": "string", "description": "Optional JSON string of extra data delivered with the notification."},
				"priority": {"type": "string", "enum": ["default", "high"], "description": "Delivery priority (default 'high')."}
			},
			"required": ["body"]
		}`),
	}
}

func (t *AppPushTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	body, _ := input["body"].(string)
	if strings.TrimSpace(body) == "" {
		return tools.ErrorResult(fmt.Errorf("body is required")), nil
	}
	title, _ := input["title"].(string)
	priority, _ := input["priority"].(string)

	var data map[string]any
	if raw, _ := input["data"].(string); strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &data)
	}

	devices, result, err := SendExpoPush(t.Store, title, body, priority, data)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("expo push failed: %w", err)), nil
	}
	if devices == 0 {
		return tools.SuccessResult(map[string]any{"sent": false, "reason": "no devices registered"}), nil
	}
	return tools.SuccessResult(map[string]any{"sent": true, "devices": devices, "result": result}), nil
}
