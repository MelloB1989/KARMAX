package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/voice"
)

// Calling somebody and actually talking to them.
//
// The integration holds the call and every byte of audio; the brain decides the
// words. This tool only chooses WHO to ring and through WHICH integration —
// WhatsApp today, whatever registers tomorrow — which is the whole point of the
// registry: adding a voice integration must not mean touching this file.

// VoiceCallTool starts a spoken conversation through a registered integration.
type VoiceCallTool struct {
	Voice *voice.Registry
}

func (t *VoiceCallTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "call.start",
		Description: "Ring somebody and TALK to them — a live two-way conversation, not a recorded " +
			"message. Use it when speaking is genuinely better than messaging: something urgent, or the " +
			"operator asked you to call. A phone call interrupts, so prefer comms.send otherwise.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"to": {"type": "string", "description": "Who to call: phone number, JID, or contact name."},
				"provider": {"type": "string", "description": "Which voice integration to call through. Omit for the default."},
				"language": {"type": "string", "description": "Speech language, e.g. 'en-IN' or 'hi-IN'. Defaults to en-IN."},
				"voice": {"type": "string", "description": "Speaker voice. Omit for the default."},
				"ring_for_seconds": {"type": "integer", "description": "How long to ring before giving up."}
			},
			"required": ["to"]
		}`),
	}
}

func (t *VoiceCallTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	to, _ := input["to"].(string)
	if strings.TrimSpace(to) == "" {
		return tools.ErrorResult(fmt.Errorf("say who to call")), nil
	}
	if t.Voice == nil {
		return tools.ErrorResult(fmt.Errorf("voice calling is not available on this instance")), nil
	}
	provider, _ := input["provider"].(string)
	opts := voice.CallOptions{}
	if v, ok := input["language"].(string); ok {
		opts.Language = strings.TrimSpace(v)
	}
	if v, ok := input["voice"].(string); ok {
		opts.Voice = strings.TrimSpace(v)
	}
	if n, ok := numberArg(input["ring_for_seconds"]); ok && n > 0 {
		opts.RingFor = int(n)
	}
	if err := t.Voice.Place(ctx, provider, strings.TrimSpace(to), opts); err != nil {
		return tools.ErrorResult(err), nil
	}
	return tools.SuccessResult(map[string]any{
		"status": "ringing",
		"to":     to,
		"note": "they will hear you when they answer; the conversation runs until either side hangs up. " +
			"whatsapp_end_call ends it from here.",
	}), nil
}
