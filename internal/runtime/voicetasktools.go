package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/tools"
)

// Task tools for operator calls: tasks created here report to the operator's DM.

type voiceTaskCreateTool struct {
	inner     *taskCreateTool
	channelID string
	target    string
	notify    func(text string) error
}

func (t *voiceTaskCreateTool) Manifest() tools.ToolManifest {
	m := t.inner.Manifest()
	var schema map[string]any
	if json.Unmarshal(m.Parameters, &schema) == nil {
		if props, ok := schema["properties"].(map[string]any); ok {
			delete(props, "channel_id")
			delete(props, "target")
			if raw, err := json.Marshal(schema); err == nil {
				m.Parameters = raw
			}
		}
	}
	m.Description += " Updates reach the operator on WhatsApp; tell the caller it is underway."
	return m
}

func (t *voiceTaskCreateTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	forced := make(map[string]any, len(in)+2)
	for k, v := range in {
		forced[k] = v
	}
	forced["channel_id"], forced["target"] = t.channelID, t.target
	res, err := t.inner.Execute(ctx, forced)
	if err != nil || res.IsError || t.notify == nil {
		return res, err
	}
	title := ""
	if out, ok := res.Output.(map[string]any); ok {
		title, _ = out["title"].(string)
	}
	if title == "" {
		title = firstLineOf(fmt.Sprint(in["goal"]))
	}
	_ = t.notify("Task started: " + strings.TrimSpace(title))
	return res, nil
}

// voiceCallerIsOperator says who is on the line; it is context for the brain, not a gate.
func voiceCallerIsOperator(peer string, isOperatorChat func(string) bool) bool {
	return strings.TrimSpace(peer) != "" && isOperatorChat != nil && isOperatorChat(peer)
}
