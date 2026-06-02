package mcp

import (
	"encoding/json"

	"github.com/MelloB1989/karmax/internal/tools"
)

type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportSSE   Transport = "sse"
	TransportWS    Transport = "ws"
)

type MCPServerConfig struct {
	ID        string            `yaml:"id"        json:"id"`
	Name      string            `yaml:"name"      json:"name"`
	Transport Transport         `yaml:"transport" json:"transport"`
	Command   []string          `yaml:"command"    json:"command,omitempty"`
	URL       string            `yaml:"url"        json:"url,omitempty"`
	Env       map[string]string `yaml:"env"        json:"env,omitempty"`
	AutoStart bool              `yaml:"auto_start" json:"auto_start"`
}

type MCPToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type MCPResult struct {
	Content []MCPContent `json:"content"`
	IsError bool         `json:"isError"`
}

type MCPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func mcpToolToToolManifest(t MCPToolInfo, serverID string) tools.ToolManifest {
	return tools.ToolManifest{
		Name:        serverID + "." + t.Name,
		Description: t.Description,
		Parameters:  t.InputSchema,
	}
}
