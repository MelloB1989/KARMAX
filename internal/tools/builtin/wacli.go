package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
)

// WacliTool gives the agent full control over the local WhatsApp bridge (wacli):
// managing the message webhook that feeds KARMAX, editing/deleting sent
// messages, reading receipts, controlling chat access (lock/unlock), inspecting
// chats/contacts/DND, and sending. It runs the wacli CLI with the provided
// subcommand args and returns its output.
type WacliTool struct {
	WacliPath string
}

func (t *WacliTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "wacli",
		Description: "Full control over the WhatsApp bridge via the local wacli CLI. Pass the subcommand and flags as the 'args' array. Use it to:\n" +
			"- Manage the message webhook that feeds KARMAX: [\"webhooks\",\"list\"]; add/scope it e.g. [\"webhooks\",\"add\",\"--url\",\"http://127.0.0.1:9090/comms/whatsapp\",\"--events\",\"incoming_message,outgoing_message\",\"--scope\",\"selected_chats\",\"--chat\",\"<jid-or-phone>\"]; remove: [\"webhooks\",\"remove\",\"<id>\"]. KARMAX consumes whatever chats this webhook is scoped to.\n" +
			"- Edit a sent message: [\"edit\",\"--chat\",\"<ref>\",\"--id\",\"<message-id>\",\"--text\",\"<new text>\"]. Delete/revoke for everyone: [\"delete\",\"--chat\",\"<ref>\",\"--id\",\"<message-id>\"].\n" +
			"- See who has received/read a message: [\"receipts\",\"--id\",\"<message-id>\"].\n" +
			"- Control which chats are exposed to automation: [\"access\",\"list\"], [\"access\",\"lock\",\"<ref>\"], [\"access\",\"unlock\",\"<ref>\"].\n" +
			"- Inspect: [\"chats\"], [\"resolve\",\"<ref>\"], [\"contacts\",\"lookup\",\"<ref>\"], [\"dnd\"].\n" +
			"Notes: wacli only sends and delivers webhooks while DND is ON. To message someone, prefer comms.send over [\"send\",...] so it routes through the channel and the operator is auto-notified. Returns wacli's JSON/text output.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"args": {
					"type": "array",
					"items": {"type": "string"},
					"description": "wacli subcommand and flags, e.g. [\"webhooks\",\"list\"] or [\"edit\",\"--chat\",\"<ref>\",\"--id\",\"<id>\",\"--text\",\"hi\"]"
				}
			},
			"required": ["args"]
		}`),
	}
}

func (t *WacliTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	args, err := toStringArgs(input["args"])
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if len(args) == 0 {
		return tools.ErrorResult(fmt.Errorf(`args is required (e.g. ["webhooks","list"])`)), nil
	}
	// Block interactive / long-running subcommands that would hang the agent;
	// those are managed by the wacli service, not invoked ad hoc.
	switch strings.ToLower(args[0]) {
	case "daemon", "tui", "login":
		return tools.ErrorResult(fmt.Errorf("wacli %q is interactive/long-running and is managed by the wacli service, not runnable as a tool", args[0])), nil
	}

	wacli := t.WacliPath
	if wacli == "" {
		wacli = defaultWacliPath()
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, wacli, args...).CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("wacli %s failed: %w\n%s", strings.Join(args, " "), err, trimmed)), nil
	}
	return tools.SuccessResult(map[string]any{
		"command": append([]string{"wacli"}, args...),
		"output":  parseWacliJSON(out),
	}), nil
}

// toStringArgs coerces the JSON args value (array of strings, or a single
// space-separated string) into a []string.
func toStringArgs(v any) ([]string, error) {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("args must be an array of strings")
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return x, nil
	case string:
		return strings.Fields(x), nil
	case nil:
		return nil, fmt.Errorf("args is required")
	default:
		return nil, fmt.Errorf("args must be an array of strings")
	}
}
