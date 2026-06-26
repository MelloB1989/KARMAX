package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
)

const (
	defaultGWSPath = "/home/mellob/.local/bin/gws"
	maxOutputLen   = 10000
	gwsTimeoutSecs = 60
)

type GoogleWorkspaceTool struct {
	GWSPath string
}

func (t *GoogleWorkspaceTool) path() string {
	if t.GWSPath != "" {
		return t.GWSPath
	}
	return defaultGWSPath
}

func (t *GoogleWorkspaceTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "google_workspace",
		Description: "Execute Google Workspace operations — manage Calendar, Drive, Gmail, Sheets, Docs, and more. Uses the Google Workspace CLI (gws). Examples: list calendar events, create docs, send emails, manage drive files.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"service": {"type": "string", "description": "Google service: drive, calendar, gmail, sheets, docs, slides, chat, admin, script"},
				"resource": {"type": "string", "description": "Resource type (e.g., 'files' for drive, 'events' for calendar, 'messages' for gmail)"},
				"method": {"type": "string", "description": "API method: list, get, create, update, delete, patch, insert, send"},
				"params": {"type": "string", "description": "JSON string of URL/query parameters"},
				"body": {"type": "string", "description": "JSON string for request body (POST/PATCH/PUT operations)"},
				"sub_resource": {"type": "string", "description": "Optional sub-resource (e.g., 'attachments' under 'messages')"}
			},
			"required": ["service", "resource", "method"]
		}`),
	}
}

func (t *GoogleWorkspaceTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	service, _ := input["service"].(string)
	resource, _ := input["resource"].(string)
	method, _ := input["method"].(string)

	if service == "" || resource == "" || method == "" {
		return tools.ErrorResult(fmt.Errorf("service, resource, and method are required")), nil
	}

	// Build command args: gws <service> <resource> [sub_resource] <method>
	args := []string{service, resource}

	if subResource, ok := input["sub_resource"].(string); ok && subResource != "" {
		args = append(args, subResource)
	}

	args = append(args, method)

	// Add params flag
	if params, ok := input["params"].(string); ok && params != "" {
		args = append(args, "--params", params)
	}

	// Add body flag
	if body, ok := input["body"].(string); ok && body != "" {
		args = append(args, "--json", body)
	}

	// Always request JSON output
	args = append(args, "--format", "json")

	timeoutCtx, cancel := context.WithTimeout(ctx, gwsTimeoutSecs*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, t.path(), args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return tools.ErrorResult(fmt.Errorf("gws exited with code %d: %s", exitErr.ExitCode(), stderr.String())), nil
		}
		return tools.ErrorResult(fmt.Errorf("failed to run gws: %w", err)), nil
	}

	output := stdout.String()
	if len(output) > maxOutputLen {
		output = output[:maxOutputLen] + "\n... [truncated]"
	}

	return tools.SuccessResult(map[string]any{
		"output": output,
	}), nil
}
