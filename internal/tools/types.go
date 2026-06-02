package tools

import (
	"context"
	"encoding/json"
)

type Tool interface {
	Manifest() ToolManifest
	Execute(ctx context.Context, input map[string]any) (ToolResult, error)
}

type ToolManifest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ToolResult struct {
	Output  any    `json:"output"`
	Error   string `json:"error,omitempty"`
	IsError bool   `json:"is_error"`
}

func ErrorResult(err error) ToolResult {
	return ToolResult{Error: err.Error(), IsError: true}
}

func SuccessResult(output any) ToolResult {
	return ToolResult{Output: output}
}
