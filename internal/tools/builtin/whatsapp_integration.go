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
	"github.com/MelloB1989/karmax/internal/tools"
)

// What is left of KARMAX's own WhatsApp tools.
//
// The reads and the send moved to wacli, which publishes them as karma tools —
// it owns WhatsApp, so it should own the surface, and a wrapper here would fall
// behind the moment it gained a feature.
//
// This one stays because it is not a fact about WhatsApp but a policy of
// KARMAX's: wacli knows which chats have webhooks, and the subtraction of the
// operator's own from that list is ours.

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
