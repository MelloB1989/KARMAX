package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/safety"
	"github.com/MelloB1989/karmax/internal/tools"
)

// The WhatsApp integration's tool surface.
//
// These were host functions in the WASM ABI — send_whatsapp, wa_chats,
// wa_messages, monitored_chats — which made one communication integration part
// of the sandbox boundary itself. Every new integration would have needed its
// own ABI, its own SDK wrappers and its own capability mapping.
//
// As tools they are reachable by the agent AND by a WASM workflow through the
// single generic `tool` host function, gated by `tool:whatsapp.*` either way.
// Adding the next integration now costs no ABI at all.

// WhatsAppChatsTool lists the operator's chats as raw wacli JSON.
type WhatsAppChatsTool struct {
	WacliPath string
}

func (t *WhatsAppChatsTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "whatsapp.chats",
		Description: "List WhatsApp chats as raw JSON, with last-message timestamps. Use this to walk every chat; use whatsapp.read for a quick look at one.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"limit": {"type": "integer", "description": "Max chats to return (default 1000)."}
			}
		}`),
	}
}

func (t *WhatsAppChatsTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	limit := intArg(input["limit"], 1000)
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	out, err := runReadTool(ctx, wacliOr(t.WacliPath), "chats", "--json", "--limit", strconv.Itoa(limit))
	return rawResult(out, err)
}

// WhatsAppMessagesTool reads one chat's messages as raw wacli JSON.
type WhatsAppMessagesTool struct {
	WacliPath string
}

func (t *WhatsAppMessagesTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "whatsapp.messages",
		Description: "Read one WhatsApp chat's messages as raw JSON, optionally only the operator's own. Structured rather than formatted, for callers that parse it.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"chat": {"type": "string", "description": "Chat JID, contact name, or phone number."},
				"limit": {"type": "integer", "description": "Max messages (default 50, max 200)."},
				"from_me_only": {"type": "boolean", "description": "Only messages the operator sent."}
			},
			"required": ["chat"]
		}`),
	}
}

func (t *WhatsAppMessagesTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	chat, _ := input["chat"].(string)
	if strings.TrimSpace(chat) == "" {
		return tools.ErrorResult(fmt.Errorf("a chat is required")), nil
	}
	limit := intArg(input["limit"], 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args := []string{"messages", "--chat", chat, "--limit", strconv.Itoa(limit)}
	if b, _ := input["from_me_only"].(bool); b {
		args = append(args, "--from-me", "yes")
	}
	out, err := runReadTool(ctx, wacliOr(t.WacliPath), args...)
	// Defanged rather than fenced: this is JSON a caller unmarshals, and a
	// fence would break the parse. Defanging still stops a message body from
	// closing an enclosing fence once its text reaches a prompt.
	return rawResult(safety.Defang(out), err)
}

// WhatsAppSendTool sends as the operator, threading a reply when asked.
//
// The reply path goes through wacli's local API rather than the CLI, because
// only the API takes a reply target. Loops cannot reach that API themselves —
// it sends messages as the operator, which is exactly what a sandbox exists to
// keep out of reach — so this tool makes the call and the caller states intent.
type WhatsAppSendTool struct {
	WacliPath string
	// Send is the comms manager's send path, used when no reply is threaded.
	Send func(channelID, target, content string) error
	// ChannelID names the WhatsApp comms channel for Send.
	ChannelID func() (string, bool)
}

func (t *WhatsAppSendTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "whatsapp.send",
		Description: "Send a WhatsApp message as the operator. Set reply_to to thread it onto an existing message.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"to": {"type": "string", "description": "Chat JID or contact to send to."},
				"text": {"type": "string", "description": "The message to send."},
				"reply_to": {"type": "string", "description": "Optional message ID to reply to."}
			},
			"required": ["to", "text"]
		}`),
	}
}

func (t *WhatsAppSendTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	to, _ := input["to"].(string)
	text, _ := input["text"].(string)
	if strings.TrimSpace(to) == "" || strings.TrimSpace(text) == "" {
		return tools.ErrorResult(fmt.Errorf("to and text are both required")), nil
	}
	replyTo, _ := input["reply_to"].(string)

	if strings.TrimSpace(replyTo) == "" && t.Send != nil {
		channel := ""
		if t.ChannelID != nil {
			if id, ok := t.ChannelID(); ok {
				channel = id
			}
		}
		if err := t.Send(channel, to, text); err != nil {
			return tools.ErrorResult(err), nil
		}
		return tools.SuccessResult(map[string]any{"sent": true}), nil
	}

	payload, err := json.Marshal(map[string]string{"to": to, "text": text, "reply_to": replyTo})
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if _, err := localPost(ctx, hostpaths.WacliAPIURL()+"/send", payload); err != nil {
		return tools.ErrorResult(err), nil
	}
	return tools.SuccessResult(map[string]any{"sent": true, "replied_to": replyTo}), nil
}

// WhatsAppMonitoredTool reports which chats KARMAX is watching.
type WhatsAppMonitoredTool struct{}

func (t *WhatsAppMonitoredTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "whatsapp.monitored",
		Description: "List the WhatsApp chats KARMAX is watching, excluding the operator's own.",
		Parameters:  json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (t *WhatsAppMonitoredTool) Execute(ctx context.Context, _ map[string]any) (tools.ToolResult, error) {
	body, err := localGet(ctx, hostpaths.WacliAPIURL()+"/webhooks")
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	var resp struct {
		Webhooks []struct {
			URL      string   `json:"url"`
			ChatJIDs []string `json:"chat_jids"`
			Enabled  bool     `json:"enabled"`
		} `json:"webhooks"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return tools.ErrorResult(err), nil
	}

	operator := map[string]bool{}
	for _, c := range OperatorChats() {
		operator[NormalizeChatID(c)] = true
	}
	chats := []string{}
	for _, wh := range resp.Webhooks {
		if !wh.Enabled || !strings.Contains(wh.URL, "/comms/whatsapp") {
			continue
		}
		for _, c := range wh.ChatJIDs {
			if !operator[NormalizeChatID(c)] {
				chats = append(chats, c)
			}
		}
	}
	return tools.SuccessResult(map[string]any{"chats": chats}), nil
}

// OperatorChats is which chats are the operator's own rather than a third
// party's, from the environment the daemon was started with.
func OperatorChats() []string {
	var out []string
	for _, p := range strings.Split(os.Getenv("WHATSAPP_OPERATOR_CHATS"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// NormalizeChatID reduces a chat id or phone number to a comparable form.
func NormalizeChatID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "@:"); i >= 0 {
		s = s[:i]
	}
	return s
}

func wacliOr(path string) string {
	if strings.TrimSpace(path) != "" {
		return path
	}
	return hostpaths.Wacli()
}

func intArg(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return parsed
		}
	}
	return def
}

// runReadTool runs one read-only host command with a bounded output.
//
// The output is returned even when the command fails, because for these tools
// the output IS the diagnosis: gws exits 2 with a JSON body saying Google needs
// an interactive reauth, and a caller can only tell the operator what to do if
// it can read that. Discarding it on error turned a specific, actionable
// message into "it failed".
func runReadTool(ctx context.Context, bin string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if len(text) > 512<<10 {
		text = text[:512<<10] + "\n…truncated"
	}
	if err != nil {
		return text, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return text, nil
}

// rawResult carries a tool's output and its error together.
func rawResult(out string, err error) (tools.ToolResult, error) {
	if err != nil {
		return tools.ToolResult{Output: map[string]any{"raw": out}, Error: err.Error(), IsError: true}, nil
	}
	return tools.SuccessResult(map[string]any{"raw": out}), nil
}

// localGet and localPost reach KARMAX's own services on this machine.
//
// Deliberately NOT the loop's gated HTTP path. That one runs the SSRF guard,
// which refuses loopback — correctly, because wacli's API can send messages as
// the operator. Routing a host call through it made the host block itself:
// chat-sweep's first run failed with "monitored_chats failed" because KARMAX
// could not reach a service on the same machine. The gate belongs on the LOOP,
// not on KARMAX acting for it.
func localGet(ctx context.Context, url string) ([]byte, error) {
	return localRequest(ctx, http.MethodGet, url, nil)
}

func localPost(ctx context.Context, url string, body []byte) ([]byte, error) {
	return localRequest(ctx, http.MethodPost, url, body)
}

func localRequest(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(cctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s answered %d: %.160s", url, resp.StatusCode, out)
	}
	return out, nil
}
