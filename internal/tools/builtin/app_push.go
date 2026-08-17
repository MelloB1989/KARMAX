package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	// The same alert, again, is not news.
	//
	// An alert names a CONDITION, and a condition seen twenty times is still
	// one thing wrong. In a single day the operator was told "Google access
	// expired" thirteen times and "Loop wa-monitor failed 3 times" twelve — a
	// quarter of everything KARMAX said to them, none of it adding to the
	// first. Repeats are suppressed by title for a few hours; a genuinely new
	// condition has a different title and still gets through immediately, and
	// anything still broken after the window says so again.
	if repeatableKinds[kind] {
		if seen, err := s.NotifiedRecently(agentID, kind, title, alertRepeatWindow); err == nil && seen {
			return
		}
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

// alertRepeatWindow is how long the same alert stays suppressed. Long enough
// that an ongoing fault is reported once a session rather than once a minute,
// short enough that a condition still broken hours later is raised again.
const alertRepeatWindow = 4 * time.Hour

// repeatableKinds are the notification kinds that describe a condition rather
// than an event.
//
// Deliberately not everything. "Handled — shiva charan" and "Sent to <someone>"
// share a title across genuinely different messages, and suppressing those
// would hide real activity rather than noise; they are frequent because KARMAX
// is busy, which is a different problem with a different fix.
var repeatableKinds = map[string]bool{
	"alert":  true,
	"loop":   true,
	"update": true,
}
