package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/MelloB1989/karmax/fleet/internal/server"
)

// mcpIn and mcpOut are the MCP server's stdio; tests replace them.
var (
	mcpIn  io.Reader = os.Stdin
	mcpOut io.Writer = os.Stdout
)

// mcpCmd is `fleetctl mcp`: a stdio MCP server whose tools call fleetd, so a
// Claude Code session anywhere fleetctl works can add it:
//
//	claude mcp add --scope user fleet -- fleetctl mcp
//
// It speaks JSON-RPC 2.0, one message per line. fleetd decides who may do
// what; this only carries the calls.
func mcpCmd(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: fleetctl mcp (a stdio MCP server; claude mcp add --scope user fleet -- fleetctl mcp)")
	}
	return serveMCP(mcpIn, mcpOut)
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// The JSON-RPC errors the server answers with.
const (
	rpcParse    = -32700
	rpcNoMethod = -32601
	rpcBadParam = -32602
)

// mcpVersion is the protocol version answered when the client asks for none.
const mcpVersion = "2025-06-18"

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var mcpTools = []mcpTool{
	{
		Name: "memory_recall",
		Description: "Search the operator's long-term memory (read-only) for what they know about a person, project " +
			"or decision. Use it before starting work the operator has history with. Agents may use it only on " +
			"tasks the operator's side gave them.",
		InputSchema: map[string]any{"type": "object", "required": []string{"query"}, "properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "what to search for, in plain words"},
			"limit": map[string]any{"type": "integer", "description": "how many memories, 1-20 (default 5)"},
		}},
	},
	{
		Name: "memory_suggest",
		Description: "Suggest a durable fact for the operator's memory. It goes to the orchestrator, which alone " +
			"decides whether to save it. Never include secrets, tokens or passwords.",
		InputSchema: map[string]any{"type": "object", "required": []string{"fact"}, "properties": map[string]any{
			"fact": map[string]any{"type": "string", "description": "the fact, one or two sentences that stand alone"},
			"why":  map[string]any{"type": "string", "description": "where it came from, or why it is worth keeping"},
		}},
	},
}

func serveMCP(in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(out)
	reply := func(id json.RawMessage, result any, e *rpcErr) error {
		m := map[string]any{"jsonrpc": "2.0", "id": id}
		if e != nil {
			m["error"] = e
		} else {
			m["result"] = result
		}
		return enc.Encode(m)
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m rpcMsg
		if err := json.Unmarshal(line, &m); err != nil {
			if err := reply(json.RawMessage("null"), nil, &rpcErr{rpcParse, "not a JSON-RPC message: " + err.Error()}); err != nil {
				return err
			}
			continue
		}
		if len(m.ID) == 0 { // a notification: nothing to answer
			continue
		}
		result, e := mcpHandle(m)
		if err := reply(m.ID, result, e); err != nil {
			return err
		}
	}
	return sc.Err()
}

func mcpHandle(m rpcMsg) (any, *rpcErr) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		v := p.ProtocolVersion
		if v == "" {
			v = mcpVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fleet", "version": server.Version},
			"instructions":    "The operator's long-term memory, through fleetd: memory_recall reads it; memory_suggest proposes a fact to the orchestrator, which alone saves.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, &rpcErr{rpcBadParam, "tools/call: " + err.Error()}
		}
		var a struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
			Fact  string `json:"fact"`
			Why   string `json:"why"`
		}
		if len(p.Arguments) > 0 {
			if err := json.Unmarshal(p.Arguments, &a); err != nil {
				return nil, &rpcErr{rpcBadParam, p.Name + ": " + err.Error()}
			}
		}
		switch p.Name {
		case "memory_recall":
			if strings.TrimSpace(a.Query) == "" {
				return toolResult("", errors.New("memory_recall needs a query")), nil
			}
			c, err := newClient()
			if err != nil {
				return toolResult("", err), nil
			}
			res, err := recall(c, a.Query, a.Limit)
			if err != nil {
				return toolResult("", err), nil
			}
			var b strings.Builder
			printRecall(&b, res)
			return toolResult(b.String(), nil), nil
		case "memory_suggest":
			if strings.TrimSpace(a.Fact) == "" {
				return toolResult("", errors.New("memory_suggest needs a fact")), nil
			}
			c, err := newClient()
			if err != nil {
				return toolResult("", err), nil
			}
			msg, err := suggest(c, a.Fact, a.Why)
			return toolResult(msg, err), nil
		}
		return nil, &rpcErr{rpcBadParam, fmt.Sprintf("no tool %q (tools/list names them)", p.Name)}
	}
	return nil, &rpcErr{rpcNoMethod, fmt.Sprintf("no method %q", m.Method)}
}

// toolResult is a tool's answer; a failure is the tool's, for the model to
// read, not the protocol's.
func toolResult(text string, err error) map[string]any {
	if err != nil {
		return map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}
}
