package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/wasmloop"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// Signed WASM loops, mounted as ordinary loops.
//
// Each one becomes a loopkit.Loop whose body instantiates its module, so it
// inherits durable runs, single-flight leases, retries and dead-lettering
// without the WASM tier knowing any of that exists — the same trick recipes
// use.

// startWasmLoops loads every verified loop from the lockfile.
func (rt *KarmaxRuntime) startWasmLoops(ctx context.Context) {
	dir := wasmloop.Dir()
	in := &wasmloop.Installer{Dir: dir, Trust: rt.loopTrust()}

	entries, err := in.Installed()
	if err != nil {
		rt.log.Warn("could not read the loop lockfile", zap.Error(err))
		return
	}
	if len(entries) == 0 {
		return
	}

	for _, e := range entries {
		if !e.Enabled {
			continue
		}
		// Verified again on load, against the lockfile rather than only against
		// the file's own manifest — a wholesale replacement would otherwise be
		// self-consistent and pass.
		a, err := in.Load(e.Name)
		if err != nil {
			rt.log.Error("refusing to run an installed loop", zap.String("loop", e.Name), zap.Error(err))
			continue
		}

		subject := broker.LoopSubject(e.Name)
		rt.broker.SetTrust(subject, broker.Registry)

		runner, err := wasmloop.NewRunner(ctx, a, wasmloop.Options{
			Namespace: rt.loopNamespace(),
			Kit:       &wasmKit{rt: rt, loop: e.Name},
			Grants:    rt.broker.For(subject),
			Log:       rt.log,
			CacheDir:  filepath.Join(dir, "cache"),
		})
		if err != nil {
			rt.log.Error("a loop would not load", zap.String("loop", e.Name), zap.Error(err))
			continue
		}
		rt.wasmRunners = append(rt.wasmRunners, runner)

		l := loopkit.Loop{
			Name:    e.Name,
			Webhook: a.Manifest.Webhook,
			Events:  a.Manifest.Events,
			Run: func(c context.Context, _ loopkit.Kit) error {
				return runner.Run(c, loopRunTimeout)
			},
		}
		if a.Manifest.Schedule != "" {
			l.Schedule = loopkit.Cron(a.Manifest.Schedule)
		}
		rt.loopkitLoops[e.Name] = l

		rt.log.Info("signed loop loaded",
			zap.String("loop", e.Name), zap.String("version", e.Version),
			zap.String("trust", string(e.Tier)), zap.Strings("host", a.Manifest.Host))
	}
}

// closeWasmLoops releases the runtimes on shutdown.
func (rt *KarmaxRuntime) closeWasmLoops(ctx context.Context) {
	for _, r := range rt.wasmRunners {
		_ = r.Close(ctx)
	}
	rt.wasmRunners = nil
}

// loopTrust is the operator's configuration for which publishers count.
func (rt *KarmaxRuntime) loopTrust() wasmloop.Trust {
	return wasmloop.Trust{
		Registries:     splitCSV(os.Getenv("KARMAX_LOOP_REGISTRIES")),
		Revoked:        splitCSV(os.Getenv("KARMAX_LOOP_REVOKED")),
		AllowCommunity: strings.EqualFold(os.Getenv("KARMAX_LOOP_ALLOW_COMMUNITY"), "true"),
	}
}

func (rt *KarmaxRuntime) loopNamespace() string {
	if len(rt.cfg.Agents) > 0 && rt.cfg.Agents[0].Memory.Namespace != "" {
		return rt.cfg.Agents[0].Memory.Namespace
	}
	return rt.loopDefaultAgent
}

// wasmKit is what a module's host calls actually reach. Everything here is
// already gated by the Broker before it is invoked.
type wasmKit struct {
	rt   *KarmaxRuntime
	loop string
}

func (w *wasmKit) mem() *loopKit {
	return &loopKit{
		loopName: w.loop, agentID: w.rt.loopDefaultAgent,
		namespace: w.rt.loopNamespace(), rt: w.rt,
		mem: w.rt.memory.For(w.rt.loopDefaultAgent, w.rt.loopNamespace()),
	}
}

func (w *wasmKit) Recall(query string, limit int) ([]string, error) {
	return w.mem().Recall(query, limit)
}

func (w *wasmKit) Remember(fact string) error { return w.mem().Remember(fact) }

func (w *wasmKit) Notify(title, body string) error { return w.mem().Notify(title, body) }

func (w *wasmKit) Ask(ctx context.Context, prompt string) (string, error) {
	return w.mem().Ask(ctx, prompt)
}

func (w *wasmKit) HTTP(ctx context.Context, method, url string, headers map[string]string, body string) (string, int, error) {
	return w.mem().HTTP(ctx, method, url, headers, body)
}

func (w *wasmKit) Config(key string) string    { return w.mem().Config(key) }
func (w *wasmKit) HostTool(name string) string { return w.mem().HostTool(name) }

func (w *wasmKit) Harness(ctx context.Context, prompt string) (string, error) {
	return w.mem().Harness(ctx, prompt)
}

// Gateway lends named host tools for one call. Only what is listed here can be
// lent, so a loop cannot invent a capability by describing one.
func (w *wasmKit) Gateway(ctx context.Context, prompt string, lend ...string) (string, error) {
	var tools []loopkit.Tool
	for _, name := range lend {
		t, ok := w.rt.lendableTool(name)
		if !ok {
			w.rt.log.Warn("a loop asked to lend an unknown tool",
				zap.String("loop", w.loop), zap.String("tool", name))
			continue
		}
		tools = append(tools, t)
	}
	return w.mem().Gateway(ctx, prompt, tools...)
}

func (w *wasmKit) Summarize(ctx context.Context, prompt string) (string, error) {
	return w.mem().Summarize(ctx, prompt)
}

func (w *wasmKit) Propose(title, summary, action string) error {
	return w.mem().Propose(title, summary, action)
}

func (w *wasmKit) Remind(title, due, notes string) error {
	return w.mem().Remind(title, due, notes)
}

// SendWhatsApp sends as the operator, threading a reply when asked.
//
// The reply path goes through wacli's local API, which the loop used to call
// itself. It cannot any more — that API sends messages as the operator, which
// is exactly the thing a sandbox exists to keep out of reach — so the host
// makes the call and the loop states the intent.
func (w *wasmKit) SendWhatsApp(ctx context.Context, target, content, replyTo string) error {
	if strings.TrimSpace(replyTo) == "" {
		return w.mem().SendWhatsApp(ctx, target, content)
	}
	payload, err := json.Marshal(map[string]string{
		"to": target, "text": content, "reply_to": replyTo})
	if err != nil {
		return err
	}
	body, status, err := w.mem().HTTP(ctx, "POST", hostpaths.WacliAPIURL()+"/send",
		map[string]string{"Content-Type": "application/json"}, string(payload))
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("wacli send answered %d: %.160s", status, body)
	}
	return nil
}

func (w *wasmKit) ReadWhatsApp(ctx context.Context, chat string, limit int) (string, error) {
	return w.mem().ReadWhatsApp(ctx, chat, limit)
}

func (w *wasmKit) ShortSet(group, key, value string, ttlSeconds int) error {
	return w.mem().ShortSet(group, key, value, time.Duration(ttlSeconds)*time.Second)
}

func (w *wasmKit) ShortGet(group, key string) (string, bool, error) {
	return w.mem().ShortGet(group, key)
}

func (w *wasmKit) ShortAll(group string) ([]wasmloop.ShortMemory, error) {
	entries, err := w.mem().ShortAll(group)
	if err != nil {
		return nil, err
	}
	out := make([]wasmloop.ShortMemory, 0, len(entries))
	for _, e := range entries {
		out = append(out, wasmloop.ShortMemory{Key: e.Key, Value: e.Value})
	}
	return out, nil
}

func (w *wasmKit) ChatSummary(jid string) (*wasmloop.ChatSummary, error) {
	rec, err := w.mem().ChatSummary(jid)
	if err != nil || rec == nil {
		return nil, err
	}
	out := &wasmloop.ChatSummary{
		ChatJID: rec.ChatJID, ChatName: rec.ChatName, IsGroup: rec.IsGroup,
		Summary: rec.Summary, MessageCount: rec.MessageCount,
		OwnMessageCount: rec.OwnMessageCount, Status: rec.Status,
	}
	if !rec.LastMessageAt.IsZero() {
		out.LastMessageAt = rec.LastMessageAt.Unix()
	}
	if !rec.SummarizedAt.IsZero() {
		out.SummarizedAt = rec.SummarizedAt.Unix()
	}
	return out, nil
}

func (w *wasmKit) SaveChatSummary(c wasmloop.ChatSummary) error {
	rec := loopkit.ChatSummaryRecord{
		ChatJID: c.ChatJID, ChatName: c.ChatName, IsGroup: c.IsGroup, Summary: c.Summary,
		MessageCount: c.MessageCount, OwnMessageCount: c.OwnMessageCount, Status: c.Status,
	}
	if c.LastMessageAt > 0 {
		rec.LastMessageAt = time.Unix(c.LastMessageAt, 0)
	}
	if c.SummarizedAt > 0 {
		rec.SummarizedAt = time.Unix(c.SummarizedAt, 0)
	}
	return w.mem().SaveChatSummary(rec)
}

func (w *wasmKit) RunLoop(name string) error { return w.mem().RunLoop(name) }

func (w *wasmKit) ShortForget(group, key string) error {
	return w.mem().ShortForget(group, key)
}

// OperatorChats replaces the WHATSAPP_OPERATOR_CHATS lookup a compiled-in loop
// used to do for itself.
func (w *wasmKit) OperatorChats() []string {
	return splitCSV(os.Getenv("WHATSAPP_OPERATOR_CHATS"))
}

// MonitoredChats returns the chats KARMAX watches, minus the operator's own.
//
// This used to be the loop's job: it fetched wacli's /webhooks over HTTP. Loops
// can no longer reach localhost — that API sends messages as the operator — so
// the host does the fetch and hands back the answer.
func (w *wasmKit) MonitoredChats(ctx context.Context) ([]string, error) {
	k := w.mem()
	body, status, err := k.HTTP(ctx, "GET", hostpaths.WacliAPIURL()+"/webhooks", nil, "")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("wacli /webhooks answered %d", status)
	}
	var resp struct {
		Webhooks []struct {
			URL      string   `json:"url"`
			ChatJIDs []string `json:"chat_jids"`
			Enabled  bool     `json:"enabled"`
		} `json:"webhooks"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, err
	}

	operator := map[string]bool{}
	for _, c := range w.OperatorChats() {
		operator[normalizeChatID(c)] = true
	}
	var out []string
	for _, wh := range resp.Webhooks {
		if !wh.Enabled || !strings.Contains(wh.URL, "/comms/whatsapp") {
			continue
		}
		for _, c := range wh.ChatJIDs {
			if !operator[normalizeChatID(c)] {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// normalizeChatID reduces a chat id or phone number to a comparable form.
func normalizeChatID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "@:"); i >= 0 {
		s = s[:i]
	}
	return s
}

// The named reads that replace a loop's exec.
//
// A compiled-in loop ran `wacli messages` and `gws chat spaces list` itself. A
// sandboxed one cannot, and the replacement is deliberately not "run this
// command" — it is a fixed set of reads, each of which is a capability the
// operator can grant or withhold. "May read your WhatsApp messages" is a
// permission somebody can reason about; "may run wacli" is not.

func (w *wasmKit) WhatsAppChats(ctx context.Context, limit int) (string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	return runHostTool(ctx, hostpaths.Wacli(), "chats", "--json", "--limit", strconv.Itoa(limit))
}

func (w *wasmKit) WhatsAppMessages(ctx context.Context, chat string, limit int, fromMeOnly bool) (string, error) {
	if strings.TrimSpace(chat) == "" {
		return "", fmt.Errorf("a chat is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args := []string{"messages", "--chat", chat, "--limit", strconv.Itoa(limit)}
	if fromMeOnly {
		args = append(args, "--from-me", "yes")
	}
	return runHostTool(ctx, hostpaths.Wacli(), args...)
}

func (w *wasmKit) GoogleChatSpaces(ctx context.Context) (string, error) {
	return runHostTool(ctx, hostpaths.GWS(), "chat", "spaces", "list", "--format", "json")
}

// runHostTool runs one read-only host command with a bounded output.
func runHostTool(ctx context.Context, bin string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", filepath.Base(bin), args[0], err)
	}
	const max = 512 << 10
	if len(out) > max {
		out = out[:max]
	}
	return string(out), nil
}

// lendableTool returns a host tool a loop may lend to the model for one call.
//
// The set is closed and read-only. wa-monitor used to build this itself with
// its own allowlist; having one copy in the host means the rule cannot drift
// between loops, and a loop cannot widen it.
func (rt *KarmaxRuntime) lendableTool(name string) (loopkit.Tool, bool) {
	switch name {
	case "wacli":
		return loopkit.Tool{
			Name: "wacli",
			Description: "Read-only WhatsApp access: messages, chats, resolve, contacts, receipts. " +
				"Cannot send — sending is a separate, approved action.",
			Schema: []byte(`{"type":"object","properties":{"args":{"type":"array","items":{"type":"string"},` +
				`"description":"e.g. [\"messages\",\"--chat\",\"<name>\",\"--limit\",\"15\"]"}},"required":["args"]}`),
			Run: func(ctx context.Context, in map[string]any) (string, error) {
				raw, _ := in["args"].([]any)
				args := make([]string, 0, len(raw))
				for _, a := range raw {
					if s, ok := a.(string); ok && strings.TrimSpace(s) != "" {
						args = append(args, s)
					}
				}
				if len(args) == 0 {
					return "", fmt.Errorf("args is required")
				}
				switch args[0] {
				case "messages", "chats", "resolve", "contacts", "receipts":
				default:
					return "", fmt.Errorf("%q is not permitted here — this tool is read-only", args[0])
				}
				return runHostTool(ctx, hostpaths.Wacli(), args...)
			},
		}, true
	}
	return loopkit.Tool{}, false
}
