package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/MelloB1989/karmax/internal/tools"
)

type NotifyTool struct{}

func (t *NotifyTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "notify.send",
		Description: "Send a desktop notification",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string"},
				"body": {"type": "string"},
				"urgency": {"type": "string", "enum": ["low", "normal", "critical"]}
			},
			"required": ["title", "body"]
		}`),
	}
}

func (t *NotifyTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	title, _ := input["title"].(string)
	body, _ := input["body"].(string)
	urgency, _ := input["urgency"].(string)

	if title == "" || body == "" {
		return tools.ErrorResult(fmt.Errorf("title and body are required")), nil
	}
	if urgency == "" {
		urgency = "normal"
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "notify-send", "-u", urgency, title, body)
	case "darwin":
		script := fmt.Sprintf(`display notification "%s" with title "%s"`, body, title)
		cmd = exec.CommandContext(ctx, "osascript", "-e", script)
	default:
		return tools.SuccessResult(map[string]any{
			"sent":    false,
			"message": "desktop notifications not supported on " + runtime.GOOS,
		}), nil
	}

	if err := cmd.Run(); err != nil {
		return tools.ErrorResult(err), nil
	}

	return tools.SuccessResult(map[string]any{"sent": true}), nil
}
