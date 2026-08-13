package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/MelloB1989/karmax/internal/comms"

	"github.com/MelloB1989/karmax/internal/tools"
)

type CommsSendTool struct {
	// SendFunc sends a message via the specified channel.
	// Accepts channelID, target, and content. Using a function type
	// avoids circular imports with the comms package.
	SendFunc func(channelID, target, content string) error
	// DefaultChannelID resolves the channel to use when the caller omits
	// channel_id (injected by the runtime; never a hardcoded name).
	DefaultChannelID func() (string, bool)
	// KnownChannelID reports whether a string names a registered channel.
	// Used to catch a channel id passed where a recipient belongs — the two
	// arrive side by side in every event payload and are easy to swap.
	KnownChannelID func(string) bool
}

func (t *CommsSendTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "comms.send",
		Description: "Send a message via a communication channel (WhatsApp, Discord, etc.). Omit channel_id to use the default channel. " +
			"On WhatsApp the target may be a contact or group NAME — it is resolved before sending. If the name matches " +
			"several conversations nothing is sent and you get the candidates back; retry with one of their jids.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"channel_id": {"type": "string", "description": "Which channel to send THROUGH (e.g. 'whatsapp-main'). Optional: defaults to the primary channel. Never put this in 'target'."},
				"target": {"type": "string", "description": "WHO to send to: a chat JID, a phone number, or a contact/group name. When replying, use the incoming event's 'channel_id' field."},
				"content": {"type": "string", "description": "The message content to send"}
			},
			"required": ["target", "content"]
		}`),
	}
}

func (t *CommsSendTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	channelID, _ := input["channel_id"].(string)
	if channelID == "" && t.DefaultChannelID != nil {
		if id, ok := t.DefaultChannelID(); ok {
			channelID = id
		}
	}
	if channelID == "" {
		return tools.ErrorResult(fmt.Errorf("channel_id is required (no default channel is registered)")), nil
	}

	target, _ := input["target"].(string)
	if target == "" {
		return tools.ErrorResult(fmt.Errorf("target is required")), nil
	}
	// Every comms.message event carries channel_id (the chat) and
	// karmax_channel_id (the transport) next to each other, and reaching for the
	// wrong one sends the transport's name to WhatsApp as if it were a person.
	// wacli answers "no matches found for \"whatsapp-main\"", which the send
	// path escalated into a critical alert to the operator — a delivery failure
	// reported as a system fault, for what is a caller mistake.
	if t.KnownChannelID != nil && t.KnownChannelID(target) {
		return tools.ErrorResult(fmt.Errorf(
			"%q is a channel, not a recipient — it names HOW to send, not WHO to. "+
				"Pass the conversation as target: for a reply use the incoming event's 'channel_id' "+
				"(the chat JID or phone number), and put %q in 'channel_id' instead",
			target, target)), nil
	}

	content, _ := input["content"].(string)
	if content == "" {
		return tools.ErrorResult(fmt.Errorf("content is required")), nil
	}

	if t.SendFunc == nil {
		return tools.ErrorResult(fmt.Errorf("comms send function not configured")), nil
	}

	if err := t.SendFunc(channelID, target, content); err != nil {
		// An ambiguous name is answerable — the candidates come back as data so
		// the next call can name one, instead of the model re-sending to the same
		// unresolvable string or giving up on a message it could deliver.
		var ambiguous *comms.AmbiguousTargetError
		if errors.As(err, &ambiguous) {
			options := make([]map[string]any, 0, len(ambiguous.Candidates))
			for _, c := range ambiguous.Candidates {
				options = append(options, map[string]any{
					"jid": c.JID, "name": c.Name, "phone": c.Phone, "is_group": c.IsGroup,
				})
			}
			return tools.ToolResult{
				Output: map[string]any{
					"status":     "ambiguous_target",
					"target":     ambiguous.Target,
					"candidates": options,
					"hint": "nothing was sent. Retry with one candidate's jid as target. " +
						"If you cannot tell which is right, ask the operator — sending to the wrong one cannot be undone",
				},
				Error:   ambiguous.Error(),
				IsError: true,
			}, nil
		}
		return tools.ErrorResult(fmt.Errorf("failed to send message: %w", err)), nil
	}

	return tools.SuccessResult(map[string]any{
		"channel_id": channelID,
		"target":     target,
		"status":     "sent",
	}), nil
}
