package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/google/uuid"
)

// AppPushTool sends a notification to the KARMAX phone app: it is persisted to
// the in-app notification feed AND delivered as an Expo push. The feed entry
// survives even if the push itself is missed or no device is registered.
type AppPushTool struct {
	Store   *store.Store
	AgentID string
}

func (t *AppPushTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "app.push",
		Description: "Send a notification to the operator's KARMAX phone app. It is saved to the in-app notification feed AND delivered as a push. Use for proactive briefings, reminders, status updates, and alerts that should surface in the app. Always succeeds (the feed entry is saved even if no device is registered for push).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string", "description": "Notification title."},
				"body": {"type": "string", "description": "Notification body."},
				"kind": {"type": "string", "description": "Optional category: briefing, reminder, alert, update."},
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
	kind, _ := input["kind"].(string)
	priority, _ := input["priority"].(string)

	rawData, _ := input["data"].(string)
	var data map[string]any
	if strings.TrimSpace(rawData) != "" {
		_ = json.Unmarshal([]byte(rawData), &data)
	}

	// Persist to the in-app feed first so it shows even if push delivery fails.
	notifID := uuid.New().String()
	if err := t.Store.CreateNotification(store.StoredNotification{
		ID:      notifID,
		AgentID: t.AgentID,
		Kind:    kind,
		Title:   title,
		Body:    body,
		Data:    rawData,
	}); err != nil {
		return tools.ErrorResult(fmt.Errorf("save notification: %w", err)), nil
	}

	// Tag the push payload so a tap can deep-link to the notifications feed.
	if data == nil {
		data = map[string]any{}
	}
	if _, ok := data["type"]; !ok {
		data["type"] = "notification"
	}
	data["notification_id"] = notifID

	devices, _, err := SendExpoPush(t.Store, title, body, priority, data)
	return tools.SuccessResult(map[string]any{
		"saved":      true,
		"id":         notifID,
		"pushed":     err == nil && devices > 0,
		"devices":    devices,
		"push_error": errString(err),
	}), nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// PushAppNotification persists an app-feed notification and delivers it as a
// push. Reusable by non-tool code paths (e.g. the proactive "message sent"
// notice fired by the comms manager). Best-effort; never blocks the caller.
func PushAppNotification(s *store.Store, agentID, kind, title, body string) {
	if s == nil || strings.TrimSpace(body) == "" {
		return
	}
	id := uuid.New().String()
	if err := s.CreateNotification(store.StoredNotification{
		ID:      id,
		AgentID: agentID,
		Kind:    kind,
		Title:   title,
		Body:    body,
	}); err != nil {
		return
	}
	data := map[string]any{"type": "notification", "notification_id": id}
	_, _, _ = SendExpoPush(s, title, body, "default", data)
}
