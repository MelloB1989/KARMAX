package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MelloB1989/karmax/internal/tools"
)

type CommsSendTool struct {
	// SendFunc sends a message via the specified channel.
	// Accepts channelID, target, and content. Using a function type
	// avoids circular imports with the comms package.
	SendFunc func(channelID, target, content string) error
}

func (t *CommsSendTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "comms.send",
		Description: "Send a message to the user via a communication channel (Discord, WhatsApp, etc.)",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"channel_id": {"type": "string", "description": "The communication channel ID (e.g., 'discord-main')"},
				"target": {"type": "string", "description": "Target channel/user ID on the platform"},
				"content": {"type": "string", "description": "The message content to send"}
			},
			"required": ["channel_id", "target", "content"]
		}`),
	}
}

func (t *CommsSendTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	channelID, _ := input["channel_id"].(string)
	if channelID == "" {
		return tools.ErrorResult(fmt.Errorf("channel_id is required")), nil
	}

	target, _ := input["target"].(string)
	if target == "" {
		return tools.ErrorResult(fmt.Errorf("target is required")), nil
	}

	content, _ := input["content"].(string)
	if content == "" {
		return tools.ErrorResult(fmt.Errorf("content is required")), nil
	}

	if t.SendFunc == nil {
		return tools.ErrorResult(fmt.Errorf("comms send function not configured")), nil
	}

	if err := t.SendFunc(channelID, target, content); err != nil {
		return tools.ErrorResult(fmt.Errorf("failed to send message: %w", err)), nil
	}

	return tools.SuccessResult(map[string]any{
		"channel_id": channelID,
		"target":     target,
		"status":     "sent",
	}), nil
}
