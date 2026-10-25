package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
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
// what; this lists only the tools the caller's scope may use (GET
// /v1/whoami), so a guest's session is not offered the operator's.
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
	{
		Name: "karmax_tools",
		Description: "List the operator's integrations KARMAX holds (mail, calendar, drive, WhatsApp, Slack, Notion, …) " +
			"that you may call through fleetd with karmax_call, each with its class: read (runs at once) or write " +
			"(waits for the orchestrator's approval). Agents may use them only on tasks the operator's side gave them.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		Name: "karmax_call",
		Description: "Call one of the operator's integrations through fleetd, which holds KARMAX's token (you never do). " +
			"A read returns its result at once. A write (anything that sends, changes, deletes or posts) waits until " +
			"the orchestrator approves it — she may ask the operator first — then runs and returns the result; " +
			"refused or expired, it returns why. Ask only for what the task needs, with a reason someone can decide on.",
		InputSchema: map[string]any{"type": "object", "required": []string{"tool", "reason"}, "properties": map[string]any{
			"tool":    map[string]any{"type": "string", "description": "the tool's name, from karmax_tools, e.g. google.mail.search"},
			"args":    map[string]any{"type": "object", "description": "the tool's arguments, as its parameters in karmax_tools say"},
			"reason":  map[string]any{"type": "string", "description": "why the task needs this call, for whoever approves it"},
			"timeout": map[string]any{"type": "string", "description": "how long a write waits for a decision, e.g. 30m (the default)"},
		}},
	},
}

// operatorTool is a tool on the operator's personal data, which fleetd
// gives the full scope and agents (relay) on the operator's tasks alone.
func operatorTool(name string) bool {
	return strings.HasPrefix(name, "memory_") || strings.HasPrefix(name, "karmax_")
}

// mayUse is whether a scope may call a tool: what fleetd would refuse is
// left out.
func mayUse(scope, tool string) bool {
	return !operatorTool(tool) || scope == "full" || scope == "relay"
}

// mcpWho is who fleetd says the session is: asked when first needed, and
// asked again until fleetd answers.
type mcpWho struct {
	mu    sync.Mutex
	scope string
}

func (w *mcpWho) get() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.scope != "" {
		return w.scope, nil
	}
	s, err := whoScope()
	if err != nil {
		return "", err
	}
	w.scope = s
	return s, nil
}

// whoScope is this machine's scope, as fleetd says (GET /v1/whoami).
func whoScope() (string, error) {
	c, err := newClient()
	if err != nil {
		return "", err
	}
	c.timeout = 20 * time.Second
	var who struct{ Scope string }
	if err := c.do("GET", "/v1/whoami", nil, &who); err != nil {
		return "", err
	}
	if who.Scope == "" {
		return "", errors.New("fleetd did not say which scope you have")
	}
	return who.Scope, nil
}

func serveMCP(in io.Reader, out io.Writer) error {
	who := &mcpWho{}
	enc := json.NewEncoder(out)
	var mu sync.Mutex // one message out at a time
	reply := func(id json.RawMessage, result any, e *rpcErr) error {
		m := map[string]any{"jsonrpc": "2.0", "id": id}
		if e != nil {
			m["error"] = e
		} else {
			m["result"] = result
		}
		mu.Lock()
		defer mu.Unlock()
		return enc.Encode(m)
	}
	// A tool call may wait a long time (a write waits for its approval), so
	// each runs on its own and the rest of the session goes on answering.
	var calls sync.WaitGroup
	defer calls.Wait()
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
		if m.Method == "tools/call" {
			calls.Add(1)
			go func() {
				defer calls.Done()
				result, e := mcpHandle(who, m)
				_ = reply(m.ID, result, e)
			}()
			continue
		}
		result, e := mcpHandle(who, m)
		if err := reply(m.ID, result, e); err != nil {
			return err
		}
	}
	return sc.Err()
}

func mcpHandle(who *mcpWho, m rpcMsg) (any, *rpcErr) {
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
		instructions := "The operator's long-term memory and integrations, through fleetd: memory_recall reads memory; memory_suggest proposes a fact to the orchestrator, which alone saves; karmax_tools lists the integrations you may call and karmax_call calls one (a write waits for the orchestrator's approval)."
		if s, err := who.get(); err != nil {
			instructions = "fleetd cannot say who you are yet (" + err.Error() + "), so no tools are offered until it can."
		} else if !mayUse(s, "memory_recall") {
			instructions = "Your fleet scope (" + s + ") has none of these tools: the operator's memory and integrations are not for it."
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fleet", "version": server.Version},
			"instructions":    instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		// Unknown (fleetd unreachable): only what any scope may use.
		s, _ := who.get()
		tools := []mcpTool{}
		for _, t := range mcpTools {
			if mayUse(s, t.Name) {
				tools = append(tools, t)
			}
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, &rpcErr{rpcBadParam, "tools/call: " + err.Error()}
		}
		var a struct {
			Query   string         `json:"query"`
			Limit   int            `json:"limit"`
			Fact    string         `json:"fact"`
			Why     string         `json:"why"`
			Tool    string         `json:"tool"`
			Args    map[string]any `json:"args"`
			Reason  string         `json:"reason"`
			Timeout string         `json:"timeout"`
		}
		if len(p.Arguments) > 0 {
			if err := json.Unmarshal(p.Arguments, &a); err != nil {
				return nil, &rpcErr{rpcBadParam, p.Name + ": " + err.Error()}
			}
		}
		known := slices.ContainsFunc(mcpTools, func(t mcpTool) bool { return t.Name == p.Name })
		if known && operatorTool(p.Name) {
			s, err := who.get()
			if err != nil {
				return toolResult("", fmt.Errorf("%s: fleetd cannot say who you are: %v", p.Name, err)), nil
			}
			if !mayUse(s, p.Name) {
				return nil, &rpcErr{rpcBadParam, fmt.Sprintf("%s is not for scope %s: the operator's memory and integrations are for the operator and their agents", p.Name, s)}
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
		case "karmax_tools":
			c, err := newClient()
			if err != nil {
				return toolResult("", err), nil
			}
			var tools []daemon.GatewayTool
			if err := c.do("GET", "/v1/karmax/tools", nil, &tools); err != nil {
				return toolResult("", err), nil
			}
			b, _ := json.MarshalIndent(tools, "", "  ")
			return toolResult(string(b), nil), nil
		case "karmax_call":
			if strings.TrimSpace(a.Tool) == "" {
				return toolResult("", errors.New("karmax_call needs a tool (karmax_tools lists them)")), nil
			}
			timeout := daemon.ApprovalTimeout
			if a.Timeout != "" {
				d, err := time.ParseDuration(a.Timeout)
				if err != nil || d <= 0 {
					return toolResult("", fmt.Errorf("timeout %q: want a duration like 30m", a.Timeout)), nil
				}
				timeout = d
			}
			c, err := newClient()
			if err != nil {
				return toolResult("", err), nil
			}
			c.timeout = 20 * time.Minute // an approved write runs inside the wait's answer
			// stdout is the protocol: the wait's progress goes to stderr.
			rep, err := karmaxCall(context.Background(), c, myName(), a.Tool, a.Args, a.Reason, timeout, os.Stderr, sleepCtx)
			if err != nil {
				return toolResult("", err), nil
			}
			if rep.Result == nil {
				return toolResult("", fmt.Errorf("%s: fleetd returned no result", a.Tool)), nil
			}
			if !rep.Result.OK {
				return toolResult("", fmt.Errorf("%s: %s", a.Tool, rep.Result.Error)), nil
			}
			return toolResult(string(rep.Result.Output), nil), nil
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
