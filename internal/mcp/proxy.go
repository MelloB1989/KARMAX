package mcp

import (
	"context"

	"github.com/MelloB1989/karmax/internal/tools"
)

type MCPToolProxy struct {
	bridge   *MCPBridge
	serverID string
	toolName string
	manifest tools.ToolManifest
}

func (p *MCPToolProxy) Manifest() tools.ToolManifest {
	return p.manifest
}

func (p *MCPToolProxy) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	return p.bridge.CallTool(ctx, p.serverID, p.toolName, input)
}
