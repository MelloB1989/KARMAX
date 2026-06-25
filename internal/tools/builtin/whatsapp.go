package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
)

const defaultWacliPath = "/home/mellob/code/wacli/wacli"

// WhatsAppReadTool reads recent WhatsApp messages via the local wacli binary,
// giving the agent real-time awareness of the operator's conversations.
type WhatsAppReadTool struct {
	WacliPath   string // path to the wacli binary
	DefaultChat string // chat used when none is specified
}

func (t *WhatsAppReadTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "whatsapp.read",
		Description: "Read recent WhatsApp messages via wacli for real-time context. " +
			"Omit 'chat' to get the latest messages across all unlocked chats, or pass 'chat' (a contact name, phone number, or chat name) to read one conversation. " +
			"Set 'list_chats' to true to enumerate available chats instead of messages.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"chat": {"type": "string", "description": "Optional: contact name, phone number, or chat name. Omit for the latest across all chats."},
				"limit": {"type": "integer", "description": "Max messages to return (default 20, max 100)."},
				"list_chats": {"type": "boolean", "description": "If true, return the list of available chats instead of messages."}
			}
		}`),
	}
}

func (t *WhatsAppReadTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	wacli := t.WacliPath
	if wacli == "" {
		wacli = defaultWacliPath
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// list_chats mode: enumerate available chats.
	if b, _ := input["list_chats"].(bool); b {
		out, err := exec.CommandContext(cmdCtx, wacli, "chats").CombinedOutput()
		if err != nil {
			return tools.ErrorResult(fmt.Errorf("wacli chats: %w (%s)", err, strings.TrimSpace(string(out)))), nil
		}
		return tools.SuccessResult(map[string]any{
			"chats": parseWacliJSON(out),
		}), nil
	}

	limit := normalizeLimit(input["limit"])

	args := []string{"messages", "--limit", strconv.Itoa(limit)}
	chat, _ := input["chat"].(string)
	if strings.TrimSpace(chat) == "" {
		chat = t.DefaultChat
	}
	if strings.TrimSpace(chat) != "" {
		args = append(args, "--chat", chat)
	}

	out, err := exec.CommandContext(cmdCtx, wacli, args...).CombinedOutput()
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("wacli messages: %w (%s)", err, strings.TrimSpace(string(out)))), nil
	}

	return tools.SuccessResult(map[string]any{
		"chat":     chat,
		"limit":    limit,
		"messages": parseWacliJSON(out),
	}), nil
}

func normalizeLimit(v any) int {
	limit := 20
	switch n := v.(type) {
	case float64:
		limit = int(n)
	case int:
		limit = n
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			limit = parsed
		}
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	return limit
}

// parseWacliJSON best-effort parses wacli JSON output into a generic value,
// falling back to the raw trimmed string when it is not valid JSON.
func parseWacliJSON(out []byte) any {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return []any{}
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
		return v
	}
	return trimmed
}
