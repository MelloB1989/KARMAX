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

// ProposeTool creates a human-in-the-loop approval request: KARMAX proposes an
// action, it appears in the operator's phone app (with a push), and only once
// they approve does the agent execute it. This is what makes "delegate anything
// with full access" safe.
type ProposeTool struct {
	Store   *store.Store
	AgentID string
}

func (t *ProposeTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "propose",
		Description: "Request the operator's approval BEFORE a CRITICAL, hard-to-undo action. Use ONLY for: spending money / making purchases, posting publicly (social, public channels), or deleting/overwriting data. This creates a pending approval in their phone app and notifies them; do NOT perform the action yet — once they approve you'll be asked to execute it. Do NOT use it for ordinary work, INCLUDING sending WhatsApp/messages to other people — the operator has enabled act-and-inform, so just send those via comms.send and they'll automatically see what you sent. Also don't propose for: replying to the operator, briefings/notifications, calendar/reminders, reading/searching, writing local files, safe commands, or delegating coding. When unsure about a routine action, act.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string", "description": "Short action title, e.g. 'Follow up with Acme on the invoice'."},
				"summary": {"type": "string", "description": "Why this matters and the context that prompted it."},
				"action": {"type": "string", "description": "The concrete action you will take on approval, including any draft text (e.g. the exact WhatsApp message)."},
				"kind": {"type": "string", "description": "Optional category: message, schedule, purchase, task, other."},
				"urgency": {"type": "string", "enum": ["low", "normal", "high"], "description": "How time-sensitive (default normal)."}
			},
			"required": ["title", "action"]
		}`),
	}
}

func (t *ProposeTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	title, _ := input["title"].(string)
	action, _ := input["action"].(string)
	if strings.TrimSpace(title) == "" || strings.TrimSpace(action) == "" {
		return tools.ErrorResult(fmt.Errorf("title and action are required")), nil
	}
	summary, _ := input["summary"].(string)
	kind, _ := input["kind"].(string)
	urgency, _ := input["urgency"].(string)

	id := uuid.New().String()
	if err := t.Store.CreateProposal(store.StoredProposal{
		ID:             id,
		AgentID:        t.AgentID,
		Kind:           kind,
		Title:          title,
		Summary:        summary,
		ProposedAction: action,
		Status:         "pending",
	}); err != nil {
		return tools.ErrorResult(fmt.Errorf("create proposal: %w", err)), nil
	}

	priority := "high"
	if urgency == "low" {
		priority = "default"
	}
	_, _, _ = SendExpoPush(t.Store, "Approval needed", title, priority, map[string]any{
		"type":        "proposal",
		"proposal_id": id,
	})

	return tools.SuccessResult(map[string]any{
		"status":      "pending_approval",
		"proposal_id": id,
		"message":     "Proposed to the operator for approval. Wait for their decision before acting.",
	}), nil
}
