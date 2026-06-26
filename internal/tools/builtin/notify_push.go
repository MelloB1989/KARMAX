package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
)

const defaultNtfyServer = "https://ntfy.sh"

// NtfyPushTool sends a push notification to the operator's phone via ntfy.
// This is the lightweight "ping me" channel that works without a native app —
// the user just subscribes to the topic in the ntfy iOS app.
type NtfyPushTool struct {
	Server string // ntfy server, defaults to https://ntfy.sh
	Topic  string // default topic to publish to
}

func (t *NtfyPushTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "notify.push",
		Description: "Send a push notification to Nikhil's phone via ntfy. Use for urgent or time-sensitive alerts, reminders, and direct pings. priority is 1-5 (5 = max/urgent, 3 = default).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"message": {"type": "string", "description": "The notification body."},
				"title": {"type": "string", "description": "Optional title/headline."},
				"priority": {"type": "integer", "description": "1 (min) to 5 (max/urgent). Default 3."},
				"tags": {"type": "string", "description": "Optional comma-separated emoji tags, e.g. 'warning,calendar'."},
				"topic": {"type": "string", "description": "Optional ntfy topic override (defaults to the configured topic)."}
			},
			"required": ["message"]
		}`),
	}
}

func (t *NtfyPushTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	message, _ := input["message"].(string)
	if strings.TrimSpace(message) == "" {
		return tools.ErrorResult(fmt.Errorf("message is required")), nil
	}

	server := strings.TrimRight(t.Server, "/")
	if server == "" {
		server = defaultNtfyServer
	}

	topic, _ := input["topic"].(string)
	if strings.TrimSpace(topic) == "" {
		topic = t.Topic
	}
	if strings.TrimSpace(topic) == "" {
		return tools.ErrorResult(fmt.Errorf("no ntfy topic configured (set NTFY_TOPIC) and none provided")), nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, server+"/"+topic, strings.NewReader(message))
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if title, _ := input["title"].(string); strings.TrimSpace(title) != "" {
		req.Header.Set("Title", title)
	}
	if prio := ntfyPriority(input["priority"]); prio != "" {
		req.Header.Set("Priority", prio)
	}
	if tags, _ := input["tags"].(string); strings.TrimSpace(tags) != "" {
		req.Header.Set("Tags", tags)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("ntfy push failed: %w", err)), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return tools.ErrorResult(fmt.Errorf("ntfy returned status %d", resp.StatusCode)), nil
	}

	return tools.SuccessResult(map[string]any{
		"sent":  true,
		"topic": topic,
	}), nil
}

// ntfyPriority normalizes a 1-5 priority into the ntfy header value, or "" if absent/invalid.
func ntfyPriority(v any) string {
	var p int
	switch n := v.(type) {
	case float64:
		p = int(n)
	case int:
		p = n
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return ""
		}
		p = parsed
	default:
		return ""
	}
	if p < 1 || p > 5 {
		return ""
	}
	return strconv.Itoa(p)
}
