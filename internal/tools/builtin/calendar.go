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

// CalendarAddTool enqueues a calendar event for the phone app to create on-device
// (EventKit). Additive and low-risk, so it runs directly without approval.
type CalendarAddTool struct {
	Store   *store.Store
	AgentID string
}

func (t *CalendarAddTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "calendar.add",
		Description: "Add an event to Nikhil's phone calendar (it appears on his iPhone via the KARMAX app). Provide start/end as ISO-8601 with a timezone offset, e.g. 2026-06-25T09:00:00+05:30. End defaults to start + 1h. Direct, low-risk — no approval needed.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string"},
				"start": {"type": "string", "description": "ISO-8601 start datetime with timezone offset."},
				"end": {"type": "string", "description": "ISO-8601 end datetime (optional; defaults to start + 1h)."},
				"notes": {"type": "string"},
				"location": {"type": "string"},
				"alarm_minutes_before": {"type": "integer", "description": "Minutes before the event to alert (optional)."}
			},
			"required": ["title", "start"]
		}`),
	}
}

func (t *CalendarAddTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	title, _ := input["title"].(string)
	start, _ := input["start"].(string)
	if strings.TrimSpace(title) == "" || strings.TrimSpace(start) == "" {
		return tools.ErrorResult(fmt.Errorf("title and start are required")), nil
	}
	payload, _ := json.Marshal(map[string]any{
		"title":                title,
		"start":                start,
		"end":                  input["end"],
		"notes":                input["notes"],
		"location":             input["location"],
		"alarm_minutes_before": input["alarm_minutes_before"],
	})
	id := uuid.New().String()
	if err := t.Store.CreateDeviceAction(store.StoredDeviceAction{
		ID: id, AgentID: t.AgentID, Kind: "calendar_event", Payload: string(payload),
	}); err != nil {
		return tools.ErrorResult(fmt.Errorf("queue calendar event: %w", err)), nil
	}
	_, _, _ = SendExpoPush(t.Store, "📅 added to calendar", title, "default", map[string]any{"type": "device_action"})

	return tools.SuccessResult(map[string]any{
		"status": "queued", "kind": "calendar_event", "id": id,
		"message": "Event queued for Nikhil's phone calendar.",
	}), nil
}

// ReminderAddTool enqueues an iOS Reminder for the app to create on-device.
type ReminderAddTool struct {
	Store   *store.Store
	AgentID string
}

func (t *ReminderAddTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "reminder.add",
		Description: "Add a reminder to Nikhil's phone (iOS Reminders). Optional due date as ISO-8601 with timezone. Direct, low-risk — no approval needed.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string"},
				"due": {"type": "string", "description": "ISO-8601 due datetime with timezone (optional)."},
				"notes": {"type": "string"}
			},
			"required": ["title"]
		}`),
	}
}

func (t *ReminderAddTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	title, _ := input["title"].(string)
	if strings.TrimSpace(title) == "" {
		return tools.ErrorResult(fmt.Errorf("title is required")), nil
	}
	payload, _ := json.Marshal(map[string]any{
		"title": title,
		"due":   input["due"],
		"notes": input["notes"],
	})
	id := uuid.New().String()
	if err := t.Store.CreateDeviceAction(store.StoredDeviceAction{
		ID: id, AgentID: t.AgentID, Kind: "reminder", Payload: string(payload),
	}); err != nil {
		return tools.ErrorResult(fmt.Errorf("queue reminder: %w", err)), nil
	}
	_, _, _ = SendExpoPush(t.Store, "✓ reminder set", title, "default", map[string]any{"type": "device_action"})

	return tools.SuccessResult(map[string]any{
		"status": "queued", "kind": "reminder", "id": id,
		"message": "Reminder queued for Nikhil's phone.",
	}), nil
}
