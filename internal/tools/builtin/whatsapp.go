package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
)

// defaultWacliPath resolves the wacli binary when no explicit path was
// configured (env -> PATH -> well-known home locations).
func defaultWacliPath() string { return hostpaths.Wacli() }

// WhatsAppReadTool reads recent WhatsApp messages via the local wacli binary,
// giving the agent real-time awareness of the operator's conversations.
type WhatsAppReadTool struct {
	WacliPath   string // path to the wacli binary
	DefaultChat string // chat used when none is specified
	Store       *store.Store // optional: resolves WhatsApp numbers -> saved contact names
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
		wacli = defaultWacliPath()
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
			"chats": t.enrichChats(parseWacliJSON(out)),
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
		"messages": t.enrichMessages(parseWacliJSON(out)),
	}), nil
}

// jidNumber extracts the digits of a WhatsApp JID's phone part
// ("12025550123:61@s.whatsapp.net" -> "12025550123"). LID JIDs (…@lid) are
// privacy identifiers, not phone numbers, so they won't match the directory.
func jidNumber(jid string) string {
	s := jid
	if i := strings.IndexAny(s, ":@"); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (t *WhatsAppReadTool) resolveJID(jid string) string {
	if t.Store == nil {
		return ""
	}
	return t.Store.LookupContactName(jidNumber(jid))
}

// enrichMessages adds saved contact names (sender_name / chat_name) to wacli
// message objects so the agent sees people by name, not number.
func (t *WhatsAppReadTool) enrichMessages(v any) any {
	if t.Store == nil {
		return v
	}
	add := func(m map[string]any) {
		if sj, ok := m["sender_jid"].(string); ok {
			if name := t.resolveJID(sj); name != "" {
				m["sender_name"] = name
			}
		}
		if cj, ok := m["chat_jid"].(string); ok {
			if name := t.resolveJID(cj); name != "" {
				m["chat_name"] = name
			}
		}
	}
	switch x := v.(type) {
	case map[string]any:
		if msgs, ok := x["messages"].([]any); ok {
			for _, mi := range msgs {
				if m, ok := mi.(map[string]any); ok {
					add(m)
				}
			}
		}
	case []any:
		for _, mi := range x {
			if m, ok := mi.(map[string]any); ok {
				add(m)
			}
		}
	}
	return v
}

// enrichChats adds the saved contact name to DM chat objects.
func (t *WhatsAppReadTool) enrichChats(v any) any {
	if t.Store == nil {
		return v
	}
	arr, ok := v.([]any)
	if !ok {
		return v
	}
	for _, ci := range arr {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		if isGroup, _ := c["is_group"].(bool); isGroup {
			continue
		}
		if jid, ok := c["jid"].(string); ok {
			if name := t.resolveJID(jid); name != "" {
				c["contact_name"] = name
			}
		}
	}
	return v
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
