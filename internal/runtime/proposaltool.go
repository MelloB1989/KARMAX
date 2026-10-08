package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
)

// proposalDecideTool approves or rejects a pending approval on the operator's say-so.
// It is lent only to operator turns (see operatorTurnTools), never registered globally.
type proposalDecideTool struct {
	store  *store.Store
	decide func(id, decision, note, by string) error
}

func (t *proposalDecideTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "proposal.decide",
		Description: "Approve or reject a pending approval by its proposal id, exactly as the operator would in the app. Approving runs the proposed action. Use only when the operator, in this very message, clearly says to approve or reject that proposal.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "The proposal id."},
				"decision": {"type": "string", "enum": ["approve", "reject"]},
				"note": {"type": "string", "description": "Optional note or feedback from the operator."}
			},
			"required": ["id", "decision"]
		}`),
	}
}

func (t *proposalDecideTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	id, _ := in["id"].(string)
	decision, _ := in["decision"].(string)
	note, _ := in["note"].(string)
	id = strings.TrimSpace(id)
	if id == "" || (decision != "approve" && decision != "reject") {
		return tools.ErrorResult(fmt.Errorf("id and a decision of approve or reject are required")), nil
	}
	p, err := t.store.GetProposal(id)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if p == nil {
		return tools.ErrorResult(fmt.Errorf("no such proposal")), nil
	}
	if p.Status != "pending" {
		return tools.ErrorResult(fmt.Errorf("proposal is already %s", p.Status)), nil
	}
	if err := t.decide(id, decision, note, "operator (whatsapp)"); err != nil {
		return tools.ErrorResult(err), nil
	}
	return tools.SuccessResult(map[string]any{"id": id, "decision": decision, "title": p.Title}), nil
}

// isOperatorTurn is the deterministic gate: only the operator's own chat or the app.
func isOperatorTurn(evt bus.Event, isOperatorChat func(agentID, chatID string) bool) bool {
	switch evt.Kind {
	case bus.EventCommsMessage:
		chatID, _ := evt.Payload["channel_id"].(string)
		return chatID != "" && isOperatorChat(evt.AgentID, chatID)
	case "api.chat":
		return true
	}
	return false
}

// operatorTurnTools are the tools only the operator's own turns may hold.
func (rt *KarmaxRuntime) operatorTurnTools(evt bus.Event) []tools.Tool {
	if rt.console == nil || rt.store == nil || !isOperatorTurn(evt, rt.isOperatorChat) {
		return nil
	}
	return []tools.Tool{&proposalDecideTool{store: rt.store, decide: rt.console.DecideProposal}}
}
