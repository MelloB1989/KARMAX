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

// WhatsAppMonitorTool manages which WhatsApp chats KARMAX proactively monitors,
// by editing the scoped wacli webhook that feeds KARMAX. It is the reliable,
// single-purpose way for the agent to add/remove/list monitored chats (instead
// of hand-assembling raw `wacli webhooks` invocations).
type WhatsAppMonitorTool struct {
	WacliPath  string
	WebhookURL string   // the KARMAX endpoint the wacli webhook posts to
	Secret     string   // HMAC secret the webhook must be (re)created with
	Protected  []string // operator command chats that must always stay monitored
}

func (t *WhatsAppMonitorTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "whatsapp.monitor",
		Description: "Manage which WhatsApp chats KARMAX proactively monitors (and acts on for the operator). " +
			"action 'list' shows the monitored chats; 'add' starts monitoring a chat; 'remove' stops. " +
			"'chat' accepts a contact name, phone number, or JID — it is resolved automatically. " +
			"Use when the operator says things like 'keep an eye on my chat with X' or 'stop watching Y'. " +
			"Never monitor large community/promotional groups.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["list", "add", "remove"], "description": "What to do."},
				"chat": {"type": "string", "description": "Contact name, phone, or JID (required for add/remove)."}
			},
			"required": ["action"]
		}`),
	}
}

func (t *WhatsAppMonitorTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	action, _ := input["action"].(string)
	chatRef, _ := input["chat"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	chatRef = strings.TrimSpace(chatRef)

	wacli := t.WacliPath
	if wacli == "" {
		wacli = defaultWacliPath()
	}

	id, chats, err := t.currentWebhook(ctx, wacli)
	if err != nil {
		return tools.ErrorResult(err), nil
	}

	switch action {
	case "list":
		return tools.SuccessResult(map[string]any{"monitored_chats": chats}), nil

	case "add", "remove":
		if chatRef == "" {
			return tools.ErrorResult(fmt.Errorf("chat is required for %s", action)), nil
		}
		jid, name, err := t.resolveChat(ctx, wacli, chatRef)
		if err != nil {
			return tools.ErrorResult(err), nil
		}

		if action == "add" {
			for _, c := range chats {
				if normalizeMonitorID(c) == normalizeMonitorID(jid) {
					return tools.SuccessResult(fmt.Sprintf("%s (%s) is already monitored.", name, jid)), nil
				}
			}
			chats = append(chats, jid)
		} else {
			for _, p := range t.Protected {
				if normalizeMonitorID(p) == normalizeMonitorID(jid) {
					return tools.ErrorResult(fmt.Errorf("%s is the operator's own command chat and cannot be unmonitored", jid)), nil
				}
			}
			var kept []string
			removed := false
			for _, c := range chats {
				if normalizeMonitorID(c) == normalizeMonitorID(jid) {
					removed = true
					continue
				}
				kept = append(kept, c)
			}
			if !removed {
				return tools.SuccessResult(fmt.Sprintf("%s (%s) was not being monitored.", name, jid)), nil
			}
			chats = kept
		}

		if err := t.applyWebhook(ctx, wacli, id, chats); err != nil {
			return tools.ErrorResult(err), nil
		}
		verb := "Now monitoring"
		if action == "remove" {
			verb = "Stopped monitoring"
		}
		return tools.SuccessResult(map[string]any{
			"result":          fmt.Sprintf("%s %s (%s).", verb, name, jid),
			"monitored_chats": chats,
		}), nil

	default:
		return tools.ErrorResult(fmt.Errorf("unknown action %q (use list|add|remove)", action)), nil
	}
}

// currentWebhook finds the wacli webhook pointing at KARMAX and returns its id
// and chat list. id 0 means no webhook exists yet.
func (t *WhatsAppMonitorTool) currentWebhook(ctx context.Context, wacli string) (int64, []string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, wacli, "webhooks", "list").CombinedOutput()
	if err != nil {
		return 0, nil, fmt.Errorf("wacli webhooks list: %v: %s", err, strings.TrimSpace(string(out)))
	}
	var resp struct {
		Webhooks []struct {
			ID       int64    `json:"id"`
			URL      string   `json:"url"`
			ChatJIDs []string `json:"chat_jids"`
		} `json:"webhooks"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return 0, nil, fmt.Errorf("parse webhooks list: %w", err)
	}
	for _, wh := range resp.Webhooks {
		if wh.URL == t.WebhookURL {
			return wh.ID, wh.ChatJIDs, nil
		}
	}
	return 0, nil, nil
}

// resolveChat resolves a name/phone/JID reference to a concrete chat JID.
func (t *WhatsAppMonitorTool) resolveChat(ctx context.Context, wacli, ref string) (jid, name string, err error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, cmdErr := exec.CommandContext(cctx, wacli, "resolve", "--kind", "chat", ref).CombinedOutput()
	if cmdErr == nil {
		var resp struct {
			Matches []struct {
				JID  string `json:"jid"`
				Name string `json:"name"`
			} `json:"matches"`
		}
		if json.Unmarshal(out, &resp) == nil && len(resp.Matches) > 0 && resp.Matches[0].JID != "" {
			return resp.Matches[0].JID, resp.Matches[0].Name, nil
		}
	}
	// Fall back to the raw ref when it already looks like a JID or phone.
	if strings.Contains(ref, "@") || isDigits(ref) {
		return ref, ref, nil
	}
	return "", "", fmt.Errorf("could not resolve chat %q", ref)
}

// applyWebhook rewrites the KARMAX webhook with the new chat list (wacli has no
// update op, so remove + re-add with the same URL/secret).
func (t *WhatsAppMonitorTool) applyWebhook(ctx context.Context, wacli string, id int64, chats []string) error {
	if len(chats) == 0 {
		return fmt.Errorf("refusing to apply an empty monitored-chat list")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if id != 0 {
		if out, err := exec.CommandContext(cctx, wacli, "webhooks", "remove", fmt.Sprintf("%d", id)).CombinedOutput(); err != nil {
			return fmt.Errorf("remove webhook %d: %v: %s", id, err, strings.TrimSpace(string(out)))
		}
	}

	args := []string{"webhooks", "add",
		"--url", t.WebhookURL,
		"--events", "incoming_message,outgoing_message",
		"--scope", "selected_chats",
	}
	for _, c := range chats {
		args = append(args, "--chat", c)
	}
	if t.Secret != "" {
		args = append(args, "--secret", t.Secret)
	}
	if out, err := exec.CommandContext(cctx, wacli, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("add webhook: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func normalizeMonitorID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "@:"); i >= 0 {
		s = s[:i]
	}
	return s
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
