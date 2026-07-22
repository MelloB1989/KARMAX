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

	ids, chats, secured, err := t.currentWebhook(ctx, wacli)
	if err != nil {
		return tools.ErrorResult(err), nil
	}

	switch action {
	case "list":
		// If the webhook set is fragmented or any part is missing the HMAC
		// secret (a stray webhook added out-of-band → 401s that silently drop
		// that contact's messages), reconcile to a single secured webhook now.
		if (len(ids) > 1 || !secured) && len(chats) > 0 {
			if err := t.applyWebhook(ctx, wacli, ids, chats); err == nil {
				return tools.SuccessResult(map[string]any{
					"monitored_chats": chats,
					"reconciled":      fmt.Sprintf("collapsed %d webhook(s) into one secured webhook", len(ids)),
				}), nil
			}
		}
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

		if err := t.applyWebhook(ctx, wacli, ids, chats); err != nil {
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

// Reconcile enforces the single-secured-webhook invariant without the agent
// having to call the tool: if the KARMAX webhook set is fragmented (more than
// one) or any part is missing the HMAC secret, it collapses them into one
// secured webhook. Returns (changed, count-of-webhooks-before, error). A no-op
// when already healthy. Safe to call on a timer / at startup.
func (t *WhatsAppMonitorTool) Reconcile(ctx context.Context) (bool, int, error) {
	wacli := t.WacliPath
	if wacli == "" {
		wacli = defaultWacliPath()
	}
	ids, chats, secured, err := t.currentWebhook(ctx, wacli)
	if err != nil {
		return false, 0, err
	}
	if len(chats) == 0 {
		return false, len(ids), nil // nothing to protect; don't create an empty webhook
	}
	if len(ids) <= 1 && secured {
		return false, len(ids), nil // already healthy
	}
	if err := t.applyWebhook(ctx, wacli, ids, chats); err != nil {
		return false, len(ids), err
	}
	return true, len(ids), nil
}

// currentWebhook finds ALL wacli webhooks pointing at KARMAX and returns their
// ids, the union of their monitored chats (deduped), and whether EVERY matching
// webhook carries the HMAC secret. Returning all ids (not just the first) is
// what lets add/remove/list collapse a fragmented or secret-less webhook set
// back into one correct webhook — otherwise a stray webhook added out-of-band
// keeps 401'ing and its contact's messages silently vanish.
func (t *WhatsAppMonitorTool) currentWebhook(ctx context.Context, wacli string) (ids []int64, chats []string, secured bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, cmdErr := exec.CommandContext(cctx, wacli, "webhooks", "list").CombinedOutput()
	if cmdErr != nil {
		return nil, nil, false, fmt.Errorf("wacli webhooks list: %v: %s", cmdErr, strings.TrimSpace(string(out)))
	}
	var resp struct {
		Webhooks []struct {
			ID       int64    `json:"id"`
			URL      string   `json:"url"`
			ChatJIDs []string `json:"chat_jids"`
			Secret   string   `json:"secret"`
		} `json:"webhooks"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, nil, false, fmt.Errorf("parse webhooks list: %w", err)
	}
	seen := map[string]bool{}
	secured = true
	for _, wh := range resp.Webhooks {
		if wh.URL != t.WebhookURL {
			continue
		}
		ids = append(ids, wh.ID)
		if t.Secret != "" && wh.Secret != t.Secret {
			secured = false
		}
		for _, c := range wh.ChatJIDs {
			key := normalizeMonitorID(c)
			if seen[key] {
				continue
			}
			seen[key] = true
			chats = append(chats, c)
		}
	}
	if len(ids) == 0 {
		secured = false
	}
	return ids, chats, secured, nil
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

// applyWebhook rewrites the KARMAX webhook set into ONE webhook with the given
// chat list (wacli has no update op, so remove-all + re-add). Passing every
// matching id — not just one — collapses any fragmentation and guarantees the
// single survivor carries the HMAC secret.
func (t *WhatsAppMonitorTool) applyWebhook(ctx context.Context, wacli string, ids []int64, chats []string) error {
	if len(chats) == 0 {
		return fmt.Errorf("refusing to apply an empty monitored-chat list")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for _, id := range ids {
		if id == 0 {
			continue
		}
		if out, err := exec.CommandContext(cctx, wacli, "webhooks", "remove", fmt.Sprintf("%d", id)).CombinedOutput(); err != nil {
			return fmt.Errorf("remove webhook %d: %v: %s", id, err, strings.TrimSpace(string(out)))
		}
	}

	args := []string{"webhooks", "add",
		"--url", t.WebhookURL,
		"--events", "incoming_message,outgoing_message",
		"--scope", "selected_chats",
		// Keep receiving @-mentions from chats OUTSIDE this scope — the
		// wa-monitor loop decides what to do with them (reply when genuinely
		// addressed, ignore "@all" blasts in untracked groups). Without this,
		// every monitor add/remove would silently rebuild the webhook without
		// mention delivery and quietly kill that behaviour.
		"--include-mentions",
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
