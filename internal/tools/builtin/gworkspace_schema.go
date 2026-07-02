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

type GoogleWorkspaceSchemaLookupTool struct {
	GWSPath string
}

func (t *GoogleWorkspaceSchemaLookupTool) path() string {
	if t.GWSPath != "" {
		return t.GWSPath
	}
	return defaultGWSPath()
}

func (t *GoogleWorkspaceSchemaLookupTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "google_workspace.schema",
		Description: "Look up the API schema for a Google Workspace operation to understand required parameters and request body format.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"operation": {"type": "string", "description": "Operation in format 'service.resource.method' (e.g., 'calendar.events.insert', 'drive.files.list')"}
			},
			"required": ["operation"]
		}`),
	}
}

func (t *GoogleWorkspaceSchemaLookupTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	operation, _ := input["operation"].(string)
	if operation == "" {
		return tools.ErrorResult(fmt.Errorf("operation is required")), nil
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, gwsTimeoutSecs*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, t.path(), "schema", operation)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return tools.ErrorResult(fmt.Errorf("gws schema exited with code %d: %s", exitErr.ExitCode(), stderr.String())), nil
		}
		return tools.ErrorResult(fmt.Errorf("failed to run gws schema: %w", err)), nil
	}

	output := stdout.String()
	if len(output) > maxOutputLen {
		output = output[:maxOutputLen] + "\n... [truncated]"
	}

	return tools.SuccessResult(map[string]any{
		"schema": output,
	}), nil
}
