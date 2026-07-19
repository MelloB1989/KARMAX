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
		Description: "Add an event to the operator's phone calendar (it appears on his iPhone via the KARMAX app). Provide start/end as ISO-8601 with a timezone offset, e.g. 2026-06-25T09:00:00+05:30. End defaults to start + 1h. Direct, low-risk — no approval needed.",
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
		"message": "Event queued for the operator's phone calendar.",
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
		Description: "Add a reminder to the operator's phone (iOS Reminders). Optional due date as ISO-8601 with timezone. Direct, low-risk — no approval needed.",
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
		"message": "Reminder queued for the operator's phone.",
	}), nil
}

// ContactAddTool enqueues a new phone contact for the app to create on-device
// (since WhatsApp only exposes a number). Additive and low-risk — no approval.
type ContactAddTool struct {
	Store   *store.Store
	AgentID string
}

func (t *ContactAddTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "contact.add",
		Description: "Save a new contact (name + phone number) to the operator's phone via the KARMAX app — useful since WhatsApp only gives a raw number. Direct, low-risk — no approval needed.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string"},
				"phone": {"type": "string", "description": "Phone number, ideally with country code."}
			},
			"required": ["name", "phone"]
		}`),
	}
}

func (t *ContactAddTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	name, _ := input["name"].(string)
	phone, _ := input["phone"].(string)
	if strings.TrimSpace(name) == "" || strings.TrimSpace(phone) == "" {
		return tools.ErrorResult(fmt.Errorf("name and phone are required")), nil
	}
	payload, _ := json.Marshal(map[string]any{"name": name, "phone": phone})
	id := uuid.New().String()
	if err := t.Store.CreateDeviceAction(store.StoredDeviceAction{
		ID: id, AgentID: t.AgentID, Kind: "create_contact", Payload: string(payload),
	}); err != nil {
		return tools.ErrorResult(fmt.Errorf("queue contact: %w", err)), nil
	}
	_, _, _ = SendExpoPush(t.Store, "👤 contact saved", name, "default", map[string]any{"type": "device_action"})

	return tools.SuccessResult(map[string]any{
		"status": "queued", "kind": "create_contact", "id": id,
		"message": "Contact queued to save on the operator's phone.",
	}), nil
}

// ContactUpdateTool renames an existing phone contact by number (upsert: if the
// number isn't saved yet, it's created). Useful for putting a name to a raw
// WhatsApp number or correcting a saved name. Additive/low-risk — no approval.
type ContactUpdateTool struct {
	Store   *store.Store
	AgentID string
}

func (t *ContactUpdateTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "contact.update",
		Description: "Set or update the saved NAME for a phone number in the operator's phone contacts (via the KARMAX app). Use to name a raw WhatsApp number or fix a wrong name — if the number isn't saved yet it's created, otherwise its name is updated. Direct, low-risk — no approval needed.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "The name to set for this number."},
				"phone": {"type": "string", "description": "Phone number, ideally with country code."}
			},
			"required": ["name", "phone"]
		}`),
	}
}

func (t *ContactUpdateTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	name, _ := input["name"].(string)
	phone, _ := input["phone"].(string)
	if strings.TrimSpace(name) == "" || strings.TrimSpace(phone) == "" {
		return tools.ErrorResult(fmt.Errorf("name and phone are required")), nil
	}
	payload, _ := json.Marshal(map[string]any{"name": name, "phone": phone})
	id := uuid.New().String()
	if err := t.Store.CreateDeviceAction(store.StoredDeviceAction{
		ID: id, AgentID: t.AgentID, Kind: "update_contact", Payload: string(payload),
	}); err != nil {
		return tools.ErrorResult(fmt.Errorf("queue contact update: %w", err)), nil
	}
	_, _, _ = SendExpoPush(t.Store, "👤 contact updated", name, "default", map[string]any{"type": "device_action"})

	return tools.SuccessResult(map[string]any{
		"status": "queued", "kind": "update_contact", "id": id,
		"message": "Contact name queued to update on the operator's phone.",
	}), nil
}
