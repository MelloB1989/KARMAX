package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"

	"github.com/MelloB1989/karmax/internal/tools"
	"go.uber.org/zap"
)

type MCPServer struct {
	cfg       MCPServerConfig
	proc      *exec.Cmd
	stdin     *json.Encoder
	stdout    *bufio.Scanner
	tools     []MCPToolInfo
	nextID    atomic.Int64
	responses map[int]chan jsonRPCResponse
	mu        sync.Mutex
	log       *zap.Logger
	cancel    context.CancelFunc
}

type MCPBridge struct {
	servers map[string]*MCPServer
	log     *zap.Logger
	mu      sync.RWMutex
}

func NewBridge(log *zap.Logger) *MCPBridge {
	return &MCPBridge{
		servers: make(map[string]*MCPServer),
		log:     log,
	}
}

func (b *MCPBridge) AddServer(cfg MCPServerConfig) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.servers[cfg.ID] = &MCPServer{
		cfg:       cfg,
		responses: make(map[int]chan jsonRPCResponse),
		log:       b.log.With(zap.String("mcp_server", cfg.ID)),
	}
	return nil
}

func (b *MCPBridge) StartAll(ctx context.Context) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for id, srv := range b.servers {
		if !srv.cfg.AutoStart {
			continue
		}
		if err := srv.start(ctx); err != nil {
			b.log.Error("failed to start MCP server", zap.String("id", id), zap.Error(err))
		}
	}
	return nil
}

func (b *MCPBridge) GetTools(serverID string) ([]tools.Tool, error) {
	b.mu.RLock()
	srv, ok := b.servers[serverID]
	b.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("MCP server not found: %s", serverID)
	}

	result := make([]tools.Tool, 0, len(srv.tools))
	for _, t := range srv.tools {
		result = append(result, &MCPToolProxy{
			bridge:   b,
			serverID: serverID,
			manifest: mcpToolToToolManifest(t, serverID),
		})
	}
	return result, nil
}

func (b *MCPBridge) CallTool(ctx context.Context, serverID, toolName string, input map[string]any) (tools.ToolResult, error) {
	b.mu.RLock()
	srv, ok := b.servers[serverID]
	b.mu.RUnlock()

	if !ok {
		return tools.ErrorResult(fmt.Errorf("MCP server not found: %s", serverID)), nil
	}

	return srv.callTool(ctx, toolName, input)
}

func (b *MCPBridge) StopAll() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, srv := range b.servers {
		srv.stop()
	}
}

func (b *MCPBridge) ListServers() []MCPServerConfig {
	b.mu.RLock()
	defer b.mu.RUnlock()

	configs := make([]MCPServerConfig, 0, len(b.servers))
	for _, srv := range b.servers {
		configs = append(configs, srv.cfg)
	}
	return configs
}

func (b *MCPBridge) ServerToolCount() map[string]int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	counts := make(map[string]int, len(b.servers))
	for id, srv := range b.servers {
		counts[id] = len(srv.tools)
	}
	return counts
}

// MCPServer methods

func (s *MCPServer) start(ctx context.Context) error {
	if s.cfg.Transport != TransportStdio {
		s.log.Info("non-stdio MCP transport not yet implemented", zap.String("transport", string(s.cfg.Transport)))
		return nil
	}

	if len(s.cfg.Command) == 0 {
		return fmt.Errorf("no command specified for stdio MCP server %s", s.cfg.ID)
	}

	srvCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	cmd := exec.CommandContext(srvCtx, s.cfg.Command[0], s.cfg.Command[1:]...)
	cmd.Env = os.Environ()
	for k, v := range s.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stdout pipe: %w", err)
	}

	cmd.Stderr = os.Stderr

	s.proc = cmd
	s.stdin = json.NewEncoder(stdinPipe)
	s.stdout = bufio.NewScanner(stdoutPipe)
	s.stdout.Buffer(make([]byte, 1<<20), 1<<20)

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start MCP process: %w", err)
	}

	go s.readPump()

	if err := s.initialize(); err != nil {
		s.stop()
		return fmt.Errorf("MCP initialize: %w", err)
	}

	if err := s.listTools(); err != nil {
		s.log.Warn("failed to list MCP tools", zap.Error(err))
	}

	s.log.Info("MCP server started", zap.Int("tool_count", len(s.tools)))
	return nil
}

func (s *MCPServer) stop() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.proc != nil && s.proc.Process != nil {
		s.proc.Process.Kill()
	}
}

func (s *MCPServer) sendRequest(method string, params any) (json.RawMessage, error) {
	id := int(s.nextID.Add(1))

	respCh := make(chan jsonRPCResponse, 1)
	s.mu.Lock()
	s.responses[id] = respCh
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.responses, id)
		s.mu.Unlock()
	}()

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	if err := s.stdin.Encode(req); err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	resp := <-respCh
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	return resp.Result, nil
}

func (s *MCPServer) readPump() {
	for s.stdout.Scan() {
		line := s.stdout.Bytes()
		var resp jsonRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}

		s.mu.Lock()
		ch, ok := s.responses[resp.ID]
		s.mu.Unlock()

		if ok {
			ch <- resp
		}
	}
}

func (s *MCPServer) initialize() error {
	_, err := s.sendRequest("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "karmax",
			"version": "1.0.0",
		},
	})
	if err != nil {
		return err
	}

	// Send initialized notification (no response expected)
	s.stdin.Encode(jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})

	return nil
}

func (s *MCPServer) listTools() error {
	result, err := s.sendRequest("tools/list", nil)
	if err != nil {
		return err
	}

	var resp struct {
		Tools []MCPToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return fmt.Errorf("unmarshal tools: %w", err)
	}

	s.tools = resp.Tools
	return nil
}

func (s *MCPServer) callTool(ctx context.Context, name string, input map[string]any) (tools.ToolResult, error) {
	result, err := s.sendRequest("tools/call", map[string]any{
		"name":      name,
		"arguments": input,
	})
	if err != nil {
		return tools.ErrorResult(err), nil
	}

	var mcpResult MCPResult
	if err := json.Unmarshal(result, &mcpResult); err != nil {
		return tools.ErrorResult(fmt.Errorf("unmarshal result: %w", err)), nil
	}

	if mcpResult.IsError {
		errMsg := "MCP tool error"
		if len(mcpResult.Content) > 0 {
			errMsg = mcpResult.Content[0].Text
		}
		return tools.ToolResult{Error: errMsg, IsError: true}, nil
	}

	var output string
	for _, c := range mcpResult.Content {
		if c.Type == "text" {
			output += c.Text
		}
	}

	return tools.SuccessResult(output), nil
}
