package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/loopinstall"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/internal/webhook"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// startLoopkitLoops schedules every loop registered via the public loopkit SDK
// (third-party + built-in installed loops) and runs them when they fire. They
// are registered with an empty AgentID so the bus->agent router ignores them —
// the runner below executes them directly via a loopKit, bypassing the agent
// prompt path (a loopkit loop is real Go code, not a prompt).
func (rt *KarmaxRuntime) startLoopkitLoops(ctx context.Context) {
	loops := loopkit.Registered()
	if len(loops) == 0 {
		return
	}

	yamlNames := map[string]bool{}
	for _, l := range rt.cfg.Loops {
		yamlNames[l.Name] = true
	}
	disabled := loopinstall.LoadDisabledLoops()
	rt.loopDefaultAgent = ""
	if len(rt.cfg.Agents) > 0 {
		rt.loopDefaultAgent = rt.cfg.Agents[0].ID
	}

	rt.loopkitLoops = make(map[string]loopkit.Loop, len(loops))
	rt.loopWebhooks = map[string]string{}
	loopEvents := map[bus.EventKind][]string{} // event kind -> loop names
	for _, l := range loops {
		if yamlNames[l.Name] {
			rt.log.Warn("loopkit loop name clashes with a yaml loop; skipping", zap.String("loop", l.Name))
			continue
		}
		if disabled[l.Name] {
			rt.log.Info("loopkit loop disabled by operator; not scheduling", zap.String("loop", l.Name))
			continue
		}
		rt.loopkitLoops[l.Name] = l
		triggers := []string{"manual"}

		// Schedule trigger (cron/interval) → a scheduler job.
		if cron := l.Schedule.CronExpr(); cron != "" {
			if err := rt.scheduler.AddJob(scheduler.ScheduledJob{
				ID:      "loopkit:" + l.Name,
				Name:    "loopkit:" + l.Name,
				Cron:    cron,
				AgentID: "", // empty => agent router skips it; our runner handles it
				Payload: map[string]any{"loopkit": l.Name},
				Enabled: true,
			}); err != nil {
				rt.log.Error("failed to schedule loopkit loop", zap.String("loop", l.Name), zap.Error(err))
			} else {
				triggers = append(triggers, "schedule("+cron+")")
			}
		}

		// Webhook trigger → a webhook route (fires when the route is hit).
		if l.Webhook != "" {
			if err := rt.webhooks.AddRoute(webhook.WebhookRoute{Path: l.Webhook, Method: "*", AgentID: ""}); err != nil {
				rt.log.Error("failed to register loopkit webhook", zap.String("loop", l.Name), zap.String("route", l.Webhook), zap.Error(err))
			} else {
				rt.loopWebhooks[l.Webhook] = l.Name
				triggers = append(triggers, "webhook("+l.Webhook+")")
			}
		}

		// Event trigger → the loop fires on matching internal bus events
		// (event-driven, no polling).
		for _, ev := range l.Events {
			if ev = strings.TrimSpace(ev); ev != "" {
				kind := bus.EventKind(ev)
				loopEvents[kind] = append(loopEvents[kind], l.Name)
				triggers = append(triggers, "event("+ev+")")
			}
		}

		rt.log.Info("registered loopkit loop", zap.String("loop", l.Name), zap.Strings("triggers", triggers))
	}
	if len(rt.loopkitLoops) == 0 {
		return
	}

	// Scheduled-job fires.
	subSched, cancelSched := rt.bus.Subscribe(bus.EventScheduledJob)
	go func() {
		defer cancelSched()
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-subSched.Ch:
				if !ok {
					return
				}
				inner, _ := evt.Payload["payload"].(map[string]any)
				if inner == nil {
					continue
				}
				if name, _ := inner["loopkit"].(string); name != "" {
					if l, found := rt.loopkitLoops[name]; found {
						go rt.runLoopkitLoop(ctx, l, loopkit.Trigger{Kind: loopkit.TriggerSchedule})
					}
				}
			}
		}
	}()

	// Bus-event fires (only subscribe to the kinds some loop listens on).
	if len(loopEvents) > 0 {
		kinds := make([]bus.EventKind, 0, len(loopEvents))
		for k := range loopEvents {
			kinds = append(kinds, k)
		}
		subEvt, cancelEvt := rt.bus.Subscribe(kinds...)
		go func() {
			defer cancelEvt()
			for {
				select {
				case <-ctx.Done():
					return
				case evt, ok := <-subEvt.Ch:
					if !ok {
						return
					}
					payload := map[string]any{"event_kind": string(evt.Kind)}
					for pk, pv := range evt.Payload {
						payload[pk] = pv
					}
					for _, name := range loopEvents[evt.Kind] {
						if l, found := rt.loopkitLoops[name]; found {
							go rt.runLoopkitLoop(ctx, l, loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: payload})
						}
					}
				}
			}
		}()
	}

	// Webhook fires (only subscribe if some loop listens on a route).
	if len(rt.loopWebhooks) > 0 {
		subWh, cancelWh := rt.bus.Subscribe(bus.EventWebhookFired)
		go func() {
			defer cancelWh()
			for {
				select {
				case <-ctx.Done():
					return
				case evt, ok := <-subWh.Ch:
					if !ok {
						return
					}
					route, _ := evt.Payload["route"].(string)
					if name, ok := rt.loopWebhooks[route]; ok {
						if l, found := rt.loopkitLoops[name]; found {
							go rt.runLoopkitLoop(ctx, l, loopkit.Trigger{Kind: loopkit.TriggerWebhook, Payload: evt.Payload})
						}
					}
				}
			}
		}()
	}
}

// RunLoopByName runs a registered loopkit loop on demand (manual trigger).
// Returns false if no loop with that name is registered/enabled.
func (rt *KarmaxRuntime) RunLoopByName(name string) (bool, error) {
	l, ok := rt.loopkitLoops[name]
	if !ok {
		return false, nil
	}
	go rt.runLoopkitLoop(context.Background(), l, loopkit.Trigger{Kind: loopkit.TriggerManual})
	return true, nil
}

// pruneStaleLoopJobs removes persisted scheduler jobs for loops that no longer
// exist or are disabled — old YAML loops left in the DB, deactivated/uninstalled
// loopkit loops, or ones the operator disabled. Without this they'd reload from
// the store on every start and fire as duplicates.
func (rt *KarmaxRuntime) pruneStaleLoopJobs() {
	disabled := loopinstall.LoadDisabledLoops()
	yamlNames := map[string]bool{}
	valid := map[string]bool{}
	for _, l := range rt.cfg.Loops {
		yamlNames[l.Name] = true
		valid["loop:"+l.Name] = true
	}
	for _, l := range loopkit.Registered() {
		if yamlNames[l.Name] || disabled[l.Name] {
			continue
		}
		valid["loopkit:"+l.Name] = true
	}
	for _, j := range rt.scheduler.ListJobs() {
		if (strings.HasPrefix(j.ID, "loop:") || strings.HasPrefix(j.ID, "loopkit:")) && !valid[j.ID] {
			if err := rt.scheduler.RemoveJob(j.ID); err != nil {
				rt.log.Warn("failed to prune stale loop job", zap.String("job", j.ID), zap.Error(err))
			} else {
				rt.log.Info("pruned stale loop job", zap.String("job", j.ID))
			}
		}
	}
}

func (rt *KarmaxRuntime) runLoopkitLoop(parent context.Context, l loopkit.Loop, trigger loopkit.Trigger) {
	ctx, cancel := context.WithTimeout(parent, 12*time.Minute)
	defer cancel()

	agentID := rt.loopDefaultAgent
	ns := agentID
	if len(rt.cfg.Agents) > 0 && rt.cfg.Agents[0].Memory.Namespace != "" {
		ns = rt.cfg.Agents[0].Memory.Namespace
	}
	wacliPath := hostpaths.Wacli()

	k := &loopKit{
		loopName:  l.Name,
		agentID:   agentID,
		namespace: ns,
		rt:        rt,
		mem:       rt.memory.For(agentID, ns),
		wacliPath: wacliPath,
		trigger:   trigger,
	}
	rt.log.Info("running loopkit loop", zap.String("loop", l.Name), zap.String("trigger", trigger.Kind))
	if err := l.Run(ctx, k); err != nil {
		rt.log.Warn("loopkit loop failed", zap.String("loop", l.Name), zap.Error(err))
		return
	}
	rt.log.Info("loopkit loop done", zap.String("loop", l.Name))
}

// loopKit implements loopkit.Kit, exposing KARMAX capabilities to authored loops.
type loopKit struct {
	loopName  string
	agentID   string
	namespace string
	rt        *KarmaxRuntime
	mem       *memory.Manager
	wacliPath string
	trigger   loopkit.Trigger
}

func (k *loopKit) Trigger() loopkit.Trigger { return k.trigger }

func (k *loopKit) Ask(ctx context.Context, prompt string) (string, error) {
	ag, ok := k.rt.agents.Get(k.agentID)
	if !ok || ag == nil {
		return "", fmt.Errorf("agent %q unavailable", k.agentID)
	}
	return ag.Chat(ctx, prompt)
}

func (k *loopKit) Harness(ctx context.Context, prompt string) (string, error) {
	tool := &builtin.ClaudeCodeTool{Store: k.rt.store, AgentID: k.agentID, Namespace: k.namespace}
	// Loop work is one-off: no follow-up value in keeping the session around.
	res, err := tool.Execute(ctx, map[string]any{"prompt": prompt, "ephemeral": true})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("harness: %s", res.Error)
	}
	return loopToolField(res, "output"), nil
}

func (k *loopKit) RunLoop(name string) error {
	ok, err := k.rt.RunLoopByName(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("loop %q not found", name)
	}
	return nil
}

func (k *loopKit) Remember(fact string) error {
	if k.mem == nil {
		return fmt.Errorf("memory unavailable")
	}
	return k.mem.Write(memory.MemoryEntry{Role: "assistant", Content: fact, Tags: []string{"loop", k.loopName}})
}

func (k *loopKit) Recall(query string, limit int) ([]string, error) {
	if k.mem == nil {
		return nil, fmt.Errorf("memory unavailable")
	}
	if limit <= 0 {
		limit = 8
	}
	res, err := k.mem.SearchSemantic(query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res))
	for _, r := range res {
		out = append(out, r.Entry.Content)
	}
	return out, nil
}

func (k *loopKit) Notify(title, body string) error {
	tool := &builtin.AppPushTool{Store: k.rt.store, AgentID: k.agentID}
	res, err := tool.Execute(context.Background(), map[string]any{"title": title, "body": body, "kind": "loop"})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("notify: %s", res.Error)
	}
	return nil
}

func (k *loopKit) Propose(title, summary, action string) error {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(action) == "" {
		return fmt.Errorf("propose: title and action are required")
	}
	_, err := builtin.CreateProposal(k.rt.store, k.agentID, "task", title, summary, action, "normal")
	return err
}

func (k *loopKit) Remind(title, due, notes string) error {
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("remind: title is required")
	}
	tool := &builtin.ReminderAddTool{Store: k.rt.store, AgentID: k.agentID}
	input := map[string]any{"title": title}
	if strings.TrimSpace(due) != "" {
		input["due"] = due
	}
	if strings.TrimSpace(notes) != "" {
		input["notes"] = notes
	}
	res, err := tool.Execute(context.Background(), input)
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("remind: %s", res.Error)
	}
	return nil
}

func (k *loopKit) SendWhatsApp(_ context.Context, target, content string) error {
	if k.rt.comms == nil {
		return fmt.Errorf("comms manager unavailable")
	}
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("empty WhatsApp target")
	}
	channelID, ok := k.rt.comms.FindChannelIDByType("whatsapp")
	if !ok {
		return fmt.Errorf("no whatsapp channel registered")
	}
	return k.rt.comms.Send(channelID, target, content)
}

func (k *loopKit) ReadWhatsApp(ctx context.Context, chat string, limit int) (string, error) {
	tool := &builtin.WhatsAppReadTool{WacliPath: k.wacliPath, Store: k.rt.store}
	in := map[string]any{}
	if strings.TrimSpace(chat) != "" {
		in["chat"] = chat
	}
	if limit > 0 {
		in["limit"] = limit
	}
	res, err := tool.Execute(ctx, in)
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("whatsapp.read: %s", res.Error)
	}
	b, _ := json.Marshal(res.Output)
	return string(b), nil
}

func (k *loopKit) HTTP(ctx context.Context, method, url string, headers map[string]string, body string) (string, int, error) {
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), url, strings.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	for hk, hv := range headers {
		req.Header.Set(hk, hv)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8MB safety cap
	return string(b), resp.StatusCode, nil
}

func (k *loopKit) HostTool(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "wacli":
		if k.wacliPath != "" {
			return k.wacliPath
		}
		return hostpaths.Wacli()
	case "gws":
		return hostpaths.GWS()
	case "karmax":
		return hostpaths.KarmaxBin()
	case "wacli-api":
		return hostpaths.WacliAPIURL()
	default:
		return name
	}
}

// Summarize runs a prompt through the first agent's summary model (falling
// back to its main model config), with the agent's fallback chain.
// Gateway runs one prompt against the agent's MAIN model (no tools) — the cheap
// path loops should try before escalating to the Claude Code harness.
func (k *loopKit) Gateway(ctx context.Context, prompt string) (string, error) {
	if len(k.rt.cfg.Agents) == 0 {
		return "", fmt.Errorf("no agent configured")
	}
	a := k.rt.cfg.Agents[0]
	var fallbacks []karmahelper.FallbackModel
	for _, fb := range a.FallbackModels {
		fallbacks = append(fallbacks, karmahelper.FallbackModel{Provider: fb.Provider, Model: fb.Model})
	}
	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:       a.Provider,
		Model:          a.Model,
		MaxTokens:      2000,
		FallbackModels: fallbacks,
	}, nil)
	resp, _, _, err := sess.Chat(ctx, prompt)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(karmahelper.CleanContent(resp)), nil
}

func (k *loopKit) Summarize(ctx context.Context, prompt string) (string, error) {
	if len(k.rt.cfg.Agents) == 0 {
		return "", fmt.Errorf("no agent configured")
	}
	a := k.rt.cfg.Agents[0]
	provider, model := a.SummaryModel.Provider, a.SummaryModel.Model
	if provider == "" {
		provider = a.Provider
	}
	if model == "" {
		model = a.Model
	}
	var fallbacks []karmahelper.FallbackModel
	for _, fb := range a.FallbackModels {
		fallbacks = append(fallbacks, karmahelper.FallbackModel{Provider: fb.Provider, Model: fb.Model})
	}
	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:       provider,
		Model:          model,
		MaxTokens:      1200,
		FallbackModels: fallbacks,
	}, nil)
	resp, _, _, err := sess.Chat(ctx, prompt)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(karmahelper.CleanContent(resp)), nil
}

func (k *loopKit) ChatSummary(jid string) (*loopkit.ChatSummaryRecord, error) {
	cs, err := k.rt.store.GetChatSummary(jid)
	if err != nil || cs == nil {
		return nil, err
	}
	return &loopkit.ChatSummaryRecord{
		ChatJID: cs.ChatJID, ChatName: cs.ChatName, IsGroup: cs.IsGroup,
		Summary: cs.Summary, MessageCount: cs.MessageCount, OwnMessageCount: cs.OwnMessageCount,
		LastMessageAt: cs.LastMessageAt, SummarizedAt: cs.SummarizedAt, Status: cs.Status,
	}, nil
}

func (k *loopKit) SaveChatSummary(rec loopkit.ChatSummaryRecord) error {
	return k.rt.store.UpsertChatSummary(store.ChatSummary{
		ChatJID: rec.ChatJID, ChatName: rec.ChatName, IsGroup: rec.IsGroup,
		Summary: rec.Summary, MessageCount: rec.MessageCount, OwnMessageCount: rec.OwnMessageCount,
		LastMessageAt: rec.LastMessageAt, SummarizedAt: rec.SummarizedAt, Status: rec.Status,
	})
}

// Config reads an install-time value from the environment, namespaced per loop:
// KARMAX_LOOP_<LOOPNAME>_<KEY> (non-alnum chars uppercased to '_').
func (k *loopKit) Config(key string) string {
	return os.Getenv("KARMAX_LOOP_" + envSanitize(k.loopName) + "_" + envSanitize(key))
}

func (k *loopKit) Logf(format string, args ...any) {
	k.rt.log.Info("[loop:" + k.loopName + "] " + fmt.Sprintf(format, args...))
}

func loopToolField(res tools.ToolResult, key string) string {
	if m, ok := res.Output.(map[string]any); ok {
		if s, ok := m[key].(string); ok {
			return s
		}
	}
	if s, ok := res.Output.(string); ok {
		return s
	}
	return ""
}

func envSanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// --- Short-term memory (scratch KV) -----------------------------------------
//
// Backed by the kv_memory table: durable across restarts, expiring via TTL, and
// partitioned into groups the calling loop names. The engine handles expiry;
// callers just set and read.

func (k *loopKit) ShortSet(group, key, value string, ttl time.Duration) error {
	if k.rt.store == nil {
		return fmt.Errorf("store unavailable")
	}
	return k.rt.store.KVSet(k.shortGroup(group), key, value, ttl)
}

func (k *loopKit) ShortGet(group, key string) (string, bool, error) {
	if k.rt.store == nil {
		return "", false, fmt.Errorf("store unavailable")
	}
	return k.rt.store.KVGet(k.shortGroup(group), key)
}

func (k *loopKit) ShortAll(group string) ([]loopkit.ShortMemory, error) {
	if k.rt.store == nil {
		return nil, fmt.Errorf("store unavailable")
	}
	entries, err := k.rt.store.KVList(k.shortGroup(group))
	if err != nil {
		return nil, err
	}
	out := make([]loopkit.ShortMemory, 0, len(entries))
	for _, e := range entries {
		out = append(out, loopkit.ShortMemory{
			Key: e.Key, Value: e.Value, ExpiresAt: e.ExpiresAt, UpdatedAt: e.UpdatedAt,
		})
	}
	return out, nil
}

func (k *loopKit) ShortForget(group, key string) error {
	if k.rt.store == nil {
		return fmt.Errorf("store unavailable")
	}
	return k.rt.store.KVDelete(k.shortGroup(group), key)
}

func (k *loopKit) ShortClear(group string) error {
	if k.rt.store == nil {
		return fmt.Errorf("store unavailable")
	}
	return k.rt.store.KVClearGroup(k.shortGroup(group))
}

// shortGroup namespaces a loop's group by the loop name, so two loops can pick
// the same group string without colliding.
func (k *loopKit) shortGroup(group string) string {
	group = strings.TrimSpace(group)
	if group == "" {
		group = "default"
	}
	return k.loopName + ":" + group
}
