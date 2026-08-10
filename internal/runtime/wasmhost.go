package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/wasmloop"
	"github.com/MelloB1989/karmax/internal/webhook"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// Signed WASM loops, mounted as ordinary loops.
//
// Each one becomes a loopkit.Loop whose body instantiates its module, so it
// inherits durable runs, single-flight leases, retries and dead-lettering
// without the WASM tier knowing any of that exists — the same trick recipes
// use.

// startWasmLoops loads every verified loop from the lockfile and wires its
// triggers, returning the event kinds any of them listen on.
//
// Wiring the triggers here is the point. Registering a loop in loopkitLoops
// only makes `karmax loops run` able to find it by name — a scheduled loop also
// needs a scheduler job, and a webhook loop a route. Without that they load,
// report themselves loaded, and never fire, which reads exactly like working.
func (rt *KarmaxRuntime) startWasmLoops(ctx context.Context) map[bus.EventKind][]string {
	events := map[bus.EventKind][]string{}
	dir := wasmloop.Dir()
	in := &wasmloop.Installer{Dir: dir, Trust: rt.loopTrust()}

	entries, err := in.Installed()
	if err != nil {
		rt.log.Warn("could not read the loop lockfile", zap.Error(err))
		return events
	}
	if len(entries) == 0 {
		return events
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

		triggers := []string{"manual"}
		if a.Manifest.Schedule != "" {
			// The same job id shape the compiled-in loops use, so the existing
			// scheduled-job consumer dispatches it without knowing the tier.
			if err := rt.scheduler.AddJob(scheduler.ScheduledJob{
				ID: "loopkit:" + e.Name, Name: "loopkit:" + e.Name,
				Cron: a.Manifest.Schedule, AgentID: "",
				Payload: map[string]any{"loopkit": e.Name}, Enabled: true,
			}); err != nil {
				rt.log.Error("a signed loop declared a schedule that is not valid",
					zap.String("loop", e.Name), zap.String("schedule", a.Manifest.Schedule), zap.Error(err))
			} else {
				triggers = append(triggers, "schedule("+a.Manifest.Schedule+")")
			}
		}
		if a.Manifest.Webhook != "" {
			if err := rt.webhooks.AddRoute(webhook.WebhookRoute{
				Path: a.Manifest.Webhook, Method: "*", AgentID: "",
			}); err != nil {
				rt.log.Error("could not register a signed loop's webhook",
					zap.String("loop", e.Name), zap.String("route", a.Manifest.Webhook), zap.Error(err))
			} else {
				rt.loopWebhooks[a.Manifest.Webhook] = e.Name
				triggers = append(triggers, "webhook("+a.Manifest.Webhook+")")
			}
		}
		for _, ev := range a.Manifest.Events {
			if ev = strings.TrimSpace(ev); ev != "" {
				events[bus.EventKind(ev)] = append(events[bus.EventKind(ev)], e.Name)
				triggers = append(triggers, "event("+ev+")")
			}
		}

		rt.log.Info("signed loop loaded",
			zap.String("loop", e.Name), zap.String("version", e.Version),
			zap.String("trust", string(e.Tier)), zap.Strings("triggers", triggers),
			zap.Strings("host", a.Manifest.Host))
	}
	return events
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
	return wasmloop.LoadTrust(wasmloop.Dir())
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
	_, err = hostPost(ctx, hostpaths.WacliAPIURL()+"/send", payload)
	return err
}

// hostGet and hostPost reach KARMAX's own local services.
//
// Deliberately NOT the loop's HTTP path. That one runs the SSRF guard and the
// Broker, which refuse loopback — correctly, because wacli's API can send
// messages as the operator. Routing a host call through it made the host block
// itself: chat-sweep's very first run failed with "monitored_chats failed"
// because the host could not reach a service running on the same machine.
//
// The gate belongs on the LOOP, not on KARMAX acting for it.
func hostGet(ctx context.Context, url string) ([]byte, error) {
	return hostRequest(ctx, http.MethodGet, url, nil)
}

func hostPost(ctx context.Context, url string, body []byte) ([]byte, error) {
	return hostRequest(ctx, http.MethodPost, url, body)
}

func hostRequest(ctx context.Context, method, url string, body []byte) ([]byte, error) {
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
	body, err := hostGet(ctx, hostpaths.WacliAPIURL()+"/webhooks")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Webhooks []struct {
			URL      string   `json:"url"`
			ChatJIDs []string `json:"chat_jids"`
			Enabled  bool     `json:"enabled"`
		} `json:"webhooks"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
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
//
// The output is returned even when the command fails, because for these tools
// the output IS the diagnosis. gws exits 2 with a JSON body saying Google needs
// an interactive reauth, and gchat-watch classifies that to tell the operator
// once with the command to run. Discarding it on error turned a specific,
// actionable message into "gchat_spaces failed:" and left the operator with a
// dead Google integration and no idea why.
func runHostTool(ctx context.Context, bin string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(cctx, bin, args...).CombinedOutput()
	const max = 512 << 10
	if len(out) > max {
		out = out[:max]
	}
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w", filepath.Base(bin), args[0], err)
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
