package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
)

// Calling somebody and actually talking to them.
//
// whatsapp_place_call plays a fixed recording and hangs up — an answering
// machine. This bridges the call to KARMAX's own relay, so the other end is the
// agent, listening and replying in real time with its memory and its tools.

const voiceCallTimeout = 30 * time.Second

// VoiceCallTool starts a spoken conversation over WhatsApp.
type VoiceCallTool struct {
	// WacliAPIURL is the local bridge that holds the call.
	WacliAPIURL string
	// RelayURL is where wacli bridges the audio: KARMAX's own /voice endpoint.
	RelayURL string
}

func (t *VoiceCallTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "call.start",
		Description: "Ring somebody on WhatsApp and TALK to them — a live two-way conversation where you " +
			"listen and reply, not a recorded message. Use it when speaking is genuinely better than " +
			"messaging: something urgent, something long to explain, or when the operator asks you to call. " +
			"A phone call interrupts, so prefer comms.send unless a call is warranted.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"to": {"type": "string", "description": "Who to call: phone number, JID, or contact name."},
				"language": {"type": "string", "description": "Speech language, e.g. 'en-IN' or 'hi-IN'. Defaults to en-IN."},
				"voice": {"type": "string", "description": "Speaker voice. Omit for the default."},
				"ring_for_seconds": {"type": "integer", "description": "How long to ring before giving up. Default is the bridge's."}
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
	if strings.TrimSpace(t.RelayURL) == "" {
		return tools.ErrorResult(fmt.Errorf(
			"voice calling is off on this instance — it needs SARVAM_API_KEY set for the relay to run")), nil
	}

	body := map[string]any{
		"to":        strings.TrimSpace(to),
		"relay_url": t.RelayURL,
		// The relay is loopback-only and authenticates nobody, so the token is
		// the protocol's field rather than a secret. Anything reaching it is
		// already on this machine.
		"token": "local",
	}
	if v, ok := input["language"].(string); ok && strings.TrimSpace(v) != "" {
		body["language"] = strings.TrimSpace(v)
	}
	if v, ok := input["voice"].(string); ok && strings.TrimSpace(v) != "" {
		body["voice"] = strings.TrimSpace(v)
	}
	if n, ok := numberArg(input["ring_for_seconds"]); ok && n > 0 {
		body["ring_for_seconds"] = int(n)
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return tools.ErrorResult(err), nil
	}

	callCtx, cancel := context.WithTimeout(ctx, voiceCallTimeout)
	defer cancel()
	endpoint := strings.TrimRight(t.WacliAPIURL, "/") + "/calls/stream"
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("could not reach the WhatsApp bridge: %w", err)), nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The bridge says which half failed — an unresolvable name, a call
		// already running, a peer who cannot be called — and that is the part
		// worth acting on.
		return tools.ErrorResult(fmt.Errorf("the call was not placed (%s): %.300s", resp.Status, raw)), nil
	}

	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return tools.SuccessResult(map[string]any{
		"status": "ringing",
		"to":     to,
		"detail": out,
		"note": "they will hear you when they answer. The conversation runs until either side hangs up; " +
			"whatsapp_end_call ends it from here.",
	}), nil
}
