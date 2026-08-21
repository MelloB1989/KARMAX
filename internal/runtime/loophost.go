package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/loopinstall"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/MelloB1989/karmax/internal/safety"
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
	// Signed loops first: they were installed deliberately, so they hold their
	// names against a compiled-in loop that happens to share one.
	loopEvents := rt.startWasmLoops(ctx) // event kind -> loop names
	for _, l := range loops {
		if yamlNames[l.Name] {
			rt.log.Warn("loopkit loop name clashes with a yaml loop; skipping", zap.String("loop", l.Name))
			continue
		}
		if disabled[l.Name] {
			rt.log.Info("loopkit loop disabled by operator; not scheduling", zap.String("loop", l.Name))
			continue
		}
		if _, taken := rt.loopkitLoops[l.Name]; taken {
			// A signed loop already holds the name. The compiled-in one does not
			// get to shadow something the operator explicitly installed.
			rt.log.Warn("compiled-in loop name clashes with an installed signed loop; skipping",
				zap.String("loop", l.Name))
			continue
		}
		rt.loopkitLoops[l.Name] = l
		rt.setLoopTrust(l.Name)
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
	rt.bus.Consume(ctx, bus.SubLoopSchedule, []bus.EventKind{bus.EventScheduledJob},
		func(_ context.Context, evt bus.Event) error {
			inner, _ := evt.Payload["payload"].(map[string]any)
			if inner == nil {
				return nil
			}
			name, _ := inner["loopkit"].(string)
			if name == "" {
				return nil
			}
			if l, found := rt.loopkitLoops[name]; found {
				go rt.runLoopDurable(ctx, l, loopkit.Trigger{Kind: loopkit.TriggerSchedule}, 1)
			}
			return nil
		})

	// Timer fires. A timer names the loop that set it, so it wakes that loop
	// and no other — unlike a plain event kind, which fans out.
	rt.bus.Consume(ctx, bus.SubLoopTimer, []bus.EventKind{bus.EventTimerFired},
		func(_ context.Context, evt bus.Event) error {
			name, _ := evt.Payload["loop"].(string)
			if name == "" {
				return nil
			}
			l, found := rt.loopkitLoops[name]
			if !found {
				// Uninstalled between arming and firing; nothing to wake.
				return nil
			}
			go rt.runLoopDurable(ctx, l, loopkit.Trigger{
				Kind: loopkit.TriggerTimer, Payload: evt.Payload,
			}, 1)
			return nil
		})

	// Bus-event fires (only subscribe to the kinds some loop listens on).
	if len(loopEvents) > 0 {
		kinds := make([]bus.EventKind, 0, len(loopEvents))
		for k := range loopEvents {
			kinds = append(kinds, k)
		}
		rt.bus.Consume(ctx, bus.SubLoopEvent, kinds,
			func(_ context.Context, evt bus.Event) error {
				payload := map[string]any{"event_kind": string(evt.Kind)}
				for pk, pv := range evt.Payload {
					payload[pk] = pv
				}
				for _, name := range loopEvents[evt.Kind] {
					if l, found := rt.loopkitLoops[name]; found {
						go rt.runLoopDurable(ctx, l, loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: payload}, 1)
					}
				}
				return nil
			})
	}

	// Webhook fires (only subscribe if some loop listens on a route).
	if len(rt.loopWebhooks) > 0 {
		rt.bus.Consume(ctx, bus.SubLoopWebhook, []bus.EventKind{bus.EventWebhookFired},
			func(_ context.Context, evt bus.Event) error {
				route, _ := evt.Payload["route"].(string)
				name, ok := rt.loopWebhooks[route]
				if !ok {
					return nil
				}
				if l, found := rt.loopkitLoops[name]; found {
					go rt.runLoopDurable(ctx, l, loopkit.Trigger{Kind: loopkit.TriggerWebhook, Payload: evt.Payload}, 1)
				}
				return nil
			})
	}
}

// setLoopTrust decides whether the broker gates this loop.
//
// Every loop today is compiled into the daemon by `loops install`, so gating one
// that has been granted nothing would break it while protecting nothing — it
// could call the same Go functions directly. So a loop with no grants is
// Ungated, and granting a loop anything is what turns enforcement on for it.
// When loops move to WASM, install writes grants from the manifest and every
// loop crosses that line at once.
func (rt *KarmaxRuntime) setLoopTrust(name string) {
	subject := broker.LoopSubject(name)
	grants, err := rt.store.Grants(subject)
	if err != nil {
		rt.log.Warn("could not read loop grants; gating it", zap.String("loop", name), zap.Error(err))
		rt.broker.SetTrust(subject, broker.Community)
		return
	}
	if len(grants) == 0 {
		rt.broker.SetTrust(subject, broker.Ungated)
		return
	}
	rt.broker.SetTrust(subject, broker.Registry)
	rt.log.Info("loop runs under capability grants",
		zap.String("loop", name), zap.Int("grants", len(grants)))
}

// startRecipes loads ~/.karmax/recipes and keeps them in step with the files.
//
// A recipe becomes a loopkit.Loop, so it gets durable runs, retries and step
// checkpointing without recipes knowing any of that exists.
func (rt *KarmaxRuntime) startRecipes(ctx context.Context) {
	dir := recipes.Dir()
	// Written before the watcher starts, so a fresh install is useful without
	// having reached the registry first. Existing files are never overwritten.
	recipes.InstallBuiltins(dir, rt.log)

	w := recipes.NewWatcher(dir, rt.log, func(loaded []recipes.Loaded) {
		rt.applyRecipes(ctx, loaded)
	})
	w.Start(ctx)

	// Registered here rather than alongside the loopkit consumers, which return
	// early when no Go loops exist — recipes must work on an instance that has
	// none, since not writing Go is the entire point of the tier.
	rt.bus.Consume(ctx, bus.SubRecipeSchedule, []bus.EventKind{bus.EventScheduledJob},
		func(_ context.Context, evt bus.Event) error {
			inner, _ := evt.Payload["payload"].(map[string]any)
			name, _ := inner["recipe"].(string)
			if name == "" {
				return nil
			}
			return rt.RunRecipe(ctx, name, loopkit.Trigger{Kind: loopkit.TriggerSchedule})
		})

	rt.bus.Consume(ctx, bus.SubRecipeTimer, []bus.EventKind{bus.EventTimerFired},
		func(_ context.Context, evt bus.Event) error {
			loop, _ := evt.Payload["loop"].(string)
			name, ok := strings.CutPrefix(loop, "recipe:")
			if !ok {
				return nil
			}
			return rt.RunRecipe(ctx, name, loopkit.Trigger{
				Kind: loopkit.TriggerTimer, Payload: evt.Payload})
		})

	rt.bus.Consume(ctx, bus.SubRecipeEvent, nil,
		func(_ context.Context, evt bus.Event) error {
			rt.recipeMu.RLock()
			var fire []string
			for name, r := range rt.recipeLoops {
				if r.On.Event != "" && bus.EventKind(r.On.Event) == evt.Kind {
					fire = append(fire, name)
				}
			}
			rt.recipeMu.RUnlock()
			for _, name := range fire {
				payload := map[string]any{"event_kind": string(evt.Kind)}
				for k, v := range evt.Payload {
					payload[k] = v
				}
				if err := rt.RunRecipe(ctx, name, loopkit.Trigger{
					Kind: loopkit.TriggerEvent, Payload: payload}); err != nil {
					rt.log.Warn("could not run a recipe", zap.String("recipe", name), zap.Error(err))
				}
			}
			return nil
		})

	rt.log.Info("watching recipes", zap.String("dir", dir))
}

// applyRecipes replaces the registered recipe loops with what is on disk now.
func (rt *KarmaxRuntime) applyRecipes(ctx context.Context, loaded []recipes.Loaded) {
	rt.recipeMu.Lock()
	defer rt.recipeMu.Unlock()

	next := map[string]*recipes.Recipe{}
	for _, r := range recipes.Valid(loaded) {
		// A signed loop was installed deliberately and carries a capability
		// manifest; a recipe is a file someone dropped in a directory. If both
		// claim a name, the one the operator approved wins and the other is
		// said out loud rather than silently shadowed.
		if _, taken := rt.loopkitLoops[r.Name]; taken {
			rt.log.Warn("recipe name is already taken by an installed loop; not loading it",
				zap.String("recipe", r.Name), zap.String("file", r.Path))
			continue
		}
		next[r.Name] = r
	}

	// Recipes that vanished stop being scheduled and lose their timers, so a
	// deleted file cannot wake anything later.
	for name := range rt.recipeLoops {
		if _, still := next[name]; !still {
			_ = rt.scheduler.RemoveJob("recipe:" + name)
			if n, err := rt.store.CancelLoopTimers("recipe:" + name); err == nil && n > 0 {
				rt.log.Info("disarmed timers for a removed recipe", zap.String("recipe", name))
			}
			rt.log.Info("recipe removed", zap.String("recipe", name))
		}
	}

	for name, r := range next {
		if prev, ok := rt.recipeLoops[name]; ok && prev.On == r.On {
			rt.recipeLoops[name] = r
			continue
		}
		rt.recipeLoops[name] = r
		if r.On.Schedule != "" {
			if err := rt.scheduler.AddJob(scheduler.ScheduledJob{
				ID: "recipe:" + name, Name: "recipe:" + name, Cron: r.On.Schedule,
				Payload: map[string]any{"recipe": name}, Enabled: true,
			}); err != nil {
				rt.log.Error("recipe schedule is not valid",
					zap.String("recipe", name), zap.String("schedule", r.On.Schedule), zap.Error(err))
			}
		}
		rt.log.Info("recipe loaded", zap.String("recipe", name), zap.Int("steps", len(r.Steps)))
	}
	rt.recipeLoops = next
}

// RunRecipe executes one recipe by name.
func (rt *KarmaxRuntime) RunRecipe(ctx context.Context, name string, trigger loopkit.Trigger) error {
	rt.recipeMu.RLock()
	r, ok := rt.recipeLoops[name]
	rt.recipeMu.RUnlock()
	if !ok {
		return fmt.Errorf("no recipe %q is loaded", name)
	}
	go rt.runLoopDurable(ctx, loopkit.Loop{
		Name: "recipe:" + name,
		Run: func(c context.Context, k loopkit.Kit) error {
			return recipes.Run(c, r, k)
		},
	}, trigger, 1)
	return nil
}

// RunLoopByName runs a registered loopkit loop on demand (manual trigger).
// Returns false if no loop with that name is registered/enabled.
func (rt *KarmaxRuntime) RunLoopByName(name string) (bool, error) {
	l, ok := rt.loopkitLoops[name]
	if !ok {
		return false, nil
	}
	go rt.runLoopDurable(context.Background(), l, loopkit.Trigger{Kind: loopkit.TriggerManual}, 1)
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
	// From what is actually LOADED, not from the compiled-in registry.
	//
	// Reading the registry meant a signed loop's job was created by
	// startWasmLoops and deleted here seconds later, because the registry knows
	// nothing about loops that arrive as WASM. The loop then reported itself
	// loaded, with its schedule in the log, and never fired.
	for name := range rt.loopkitLoops {
		if yamlNames[name] || disabled[name] {
			continue
		}
		valid["loopkit:"+name] = true
	}
	rt.recipeMu.RLock()
	for name := range rt.recipeLoops {
		valid["recipe:"+name] = true
	}
	rt.recipeMu.RUnlock()
	for _, j := range rt.scheduler.ListJobs() {
		if (strings.HasPrefix(j.ID, "loop:") || strings.HasPrefix(j.ID, "loopkit:") ||
			strings.HasPrefix(j.ID, "recipe:")) && !valid[j.ID] {
			if err := rt.scheduler.RemoveJob(j.ID); err != nil {
				rt.log.Warn("failed to prune stale loop job", zap.String("job", j.ID), zap.Error(err))
			} else {
				rt.log.Info("pruned stale loop job", zap.String("job", j.ID))
			}
			// A loop that is gone must not still be woken by its own timers.
			name := j.ID[strings.Index(j.ID, ":")+1:]
			if n, err := rt.store.CancelLoopTimers(name); err == nil && n > 0 {
				rt.log.Info("disarmed timers for a removed loop",
					zap.String("loop", name), zap.Int64("count", n))
			}
		}
	}
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
	// executionID identifies this piece of work across every attempt at it, so
	// checkpoints from a failed attempt are visible to the next one.
	executionID string
}

func (k *loopKit) Trigger() loopkit.Trigger { return k.trigger }

func (k *loopKit) Ask(ctx context.Context, prompt string) (string, error) {
	return k.AskWithTools(ctx, prompt, nil)
}

// Observe is Ask with every way of speaking taken away for the turn.
//
// The list is the outbound tool set the runtime already maintains for deciding
// whether a loop run has spoken, so a new way to send cannot be added in one
// place and forgotten in this one.
func (k *loopKit) Observe(ctx context.Context, prompt string) (string, error) {
	ag, ok := k.rt.agents.Get(k.agentID)
	if !ok || ag == nil {
		return "", fmt.Errorf("agent %q unavailable", k.agentID)
	}
	out, _, err := ag.ChatDetailedWithheld(ctx, prompt, nil, outboundTools)
	return out, err
}

// AskWithTools is Ask with tools lent to the agent for that turn — the way a
// WASM workflow hands the agent its own tools to answer with.
func (k *loopKit) AskWithTools(ctx context.Context, prompt string, lent []tools.Tool) (string, error) {
	ag, ok := k.rt.agents.Get(k.agentID)
	if !ok || ag == nil {
		return "", fmt.Errorf("agent %q unavailable", k.agentID)
	}
	out, calls, err := ag.ChatDetailed(ctx, prompt, lent)
	if err == nil {
		// Work the turn STARTED but did not finish — a background delegation —
		// completes as an event long after this returns. Recording the job now
		// is what lets the workflow's tools be there when it does.
		if len(lent) > 0 {
			k.rt.attributeJobs(k.loopName, calls)
		}
		return out, nil
	}
	// Every configured "fallback model" is reached through the same
	// ANTHROPIC_BASE_URL, so when that one process is down they all fail
	// together and the chain provides no redundancy at all — 141 loop failures
	// over three days were one dead gateway wearing a fallback chain's costume.
	//
	// The harness is a genuinely different path: a separate binary with its own
	// auth that does not touch the gateway. Only transport failures fall
	// through; a refusal or a bad prompt would fail identically on both and
	// retrying it just costs twice.
	if !isTransportFailure(err) {
		return "", err
	}
	k.Logf("model gateway unreachable (%v); falling back to the harness", err)
	out, herr := k.Harness(ctx, prompt)
	if herr != nil {
		// Report the original failure: the gateway being down is the cause,
		// and the harness error is a symptom of the workaround.
		return "", fmt.Errorf("gateway unreachable (%w); harness fallback also failed: %v", err, herr)
	}
	return out, nil
}

// isTransportFailure reports whether an error means the model could not be
// REACHED, as opposed to having been reached and having declined.
func isTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, sig := range []string{
		"connection refused", "no such host", "connection reset",
		"i/o timeout", "eof", "dial tcp", "context deadline exceeded",
		"all models failed", "502", "503", "504",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

func (k *loopKit) Harness(ctx context.Context, prompt string) (string, error) {
	tool := &builtin.ClaudeCodeTool{Store: k.rt.store, AgentID: k.agentID, Namespace: k.namespace,
		MemoryMgr: k.mem}
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
	// Two gates, and they answer different questions. safety.CheckURL asks
	// whether ANY loop should reach that address; the broker asks whether THIS
	// loop was granted that host.
	if err := safety.CheckURL(url); err != nil {
		k.rt.log.Warn("loop HTTP request refused",
			zap.String("loop", k.loopName), zap.String("url", url), zap.Error(err))
		return "", 0, err
	}
	if err := k.grants().HTTP(hostOf(url)); err != nil {
		return "", 0, err
	}
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
// loopToolAdapter exposes a loop-provided loopkit.Tool through KARMAX's tools.Tool
// interface, so a loop can lend the gateway model a capability for exactly one
// call without that tool ever being registered in the core toolset.
type loopToolAdapter struct {
	loopName string
	tool     loopkit.Tool
}

func (a *loopToolAdapter) Manifest() tools.ToolManifest {
	schema := a.tool.Schema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return tools.ToolManifest{
		Name:        a.tool.Name,
		Description: a.tool.Description,
		Parameters:  schema,
	}
}

func (a *loopToolAdapter) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	if a.tool.Run == nil {
		return tools.ErrorResult(fmt.Errorf("tool %q has no implementation", a.tool.Name)), nil
	}
	out, err := a.tool.Run(ctx, input)
	if err != nil {
		// Surface the failure TO THE MODEL so it can adapt, rather than aborting.
		return tools.ErrorResult(err), nil
	}
	return tools.SuccessResult(out), nil
}

// Gateway runs one prompt against the agent's MAIN model — the cheap path loops
// should try before escalating to the Claude Code harness. Any tools the loop
// passes live only for this call.
func (k *loopKit) Gateway(ctx context.Context, prompt string, lent ...loopkit.Tool) (string, error) {
	if len(k.rt.cfg.Agents) == 0 {
		return "", fmt.Errorf("no agent configured")
	}
	a := k.rt.cfg.Agents[0]
	var fallbacks []karmahelper.FallbackModel
	for _, fb := range a.FallbackModels {
		fallbacks = append(fallbacks, karmahelper.FallbackModel{Provider: fb.Provider, Model: fb.Model})
	}
	var lentTools []tools.Tool
	for _, t := range lent {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		lentTools = append(lentTools, &loopToolAdapter{loopName: k.loopName, tool: t})
	}
	if len(lentTools) > 0 {
		k.rt.log.Debug("loop lent tools to the gateway",
			zap.String("loop", k.loopName), zap.Int("tools", len(lentTools)))
	}
	// The cheap tier by default, and the main model only when a loop says it
	// needs it.
	//
	// Every monitored WhatsApp message went through here on the main model,
	// carrying the chat's recent history each time: measured at ~22k input
	// tokens a call, and it is the single largest line in the bill. What loops
	// actually ask of it — is this worth answering, what kind of thing is it,
	// draft an acknowledgement — is classification, which the cheap model does
	// as well for a fifth of the price. A loop that genuinely needs the big
	// model sets gateway_model: main in its config, and pays for it knowingly.
	provider, model := a.SummaryModel.Provider, a.SummaryModel.Model
	if model == "" {
		provider, model = a.Provider, a.Model
	}
	if strings.EqualFold(strings.TrimSpace(k.Config("gateway_model")), "main") {
		provider, model = a.Provider, a.Model
	}
	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Kind:           "loop-gateway",
		AgentID:        k.agentID,
		Provider:       provider,
		Model:          model,
		MaxTokens:      2000,
		FallbackModels: fallbacks,
	}, lentTools)
	resp, _, _, err := sess.Chat(ctx, prompt)
	if err != nil {
		// Same reasoning as Ask: the fallback models all share one base URL, so
		// they are not redundancy. The harness is. Lent tools are the one thing
		// that cannot cross over — the harness has its own toolset — so a call
		// that depended on them fails rather than silently answering without
		// the tools it was given.
		if isTransportFailure(err) && len(lentTools) == 0 {
			k.Logf("gateway unreachable (%v); falling back to the harness", err)
			if out, herr := k.Harness(ctx, prompt); herr == nil {
				return out, nil
			}
		}
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
		Kind:           "loop-summarize",
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
func (k *loopKit) Step(name string, fn func() (string, error)) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("step: a checkpoint needs a name")
	}
	if k.executionID == "" {
		return fn() // nothing to checkpoint against; do not pretend to memoise
	}
	if result, done, err := k.rt.store.LoopStep(k.executionID, name); err != nil {
		k.rt.log.Warn("could not read a loop checkpoint; running the step again",
			zap.String("loop", k.loopName), zap.String("step", name), zap.Error(err))
	} else if done {
		k.Logf("step %q already done; skipping", name)
		return result, nil
	}

	result, err := fn()
	if err != nil {
		return "", err
	}
	// Recorded only on success: a step that failed has not happened.
	if err := k.rt.store.SaveLoopStep(k.executionID, k.loopName, name, result); err != nil {
		k.rt.log.Warn("could not record a loop checkpoint; a retry will repeat this step",
			zap.String("loop", k.loopName), zap.String("step", name), zap.Error(err))
	}
	return result, nil
}

func (k *loopKit) Once(name string, fn func() error) error {
	_, err := k.Step(name, func() (string, error) { return "", fn() })
	return err
}

func (k *loopKit) Fence(source, content string) string {
	return safety.Fence(source, content)
}

// grants is this loop's capability handle.
func (k *loopKit) grants() *broker.Handle {
	return k.rt.broker.For(broker.LoopSubject(k.loopName))
}

func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	return strings.ToLower(u.Hostname())
}

// timerID namespaces a loop's timer ids so two loops cannot collide on "daily".
func (k *loopKit) timerID(id string) string {
	return "loop:" + k.loopName + ":" + strings.TrimSpace(id)
}

func (k *loopKit) After(id string, d time.Duration, payload map[string]any) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("after: a timer needs an id")
	}
	if d < 0 {
		return fmt.Errorf("after: %s is in the past", d)
	}
	return k.rt.clock.After(store.Timer{
		ID:      k.timerID(id),
		Kind:    string(bus.EventTimerFired),
		AgentID: k.agentID,
		Loop:    k.loopName,
		Payload: payload,
	}, d)
}

func (k *loopKit) CancelAfter(id string) error {
	return k.rt.clock.Cancel(k.timerID(id))
}

func (k *loopKit) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	// Refused rather than silently truncated by the run timeout: a loop that
	// asked to wait an hour and was cancelled at twelve minutes would look like
	// it had waited, and act on it.
	if d >= loopRunTimeout {
		return fmt.Errorf("sleep: %s is longer than a run can live (%s) — use After for waits this long",
			d, loopRunTimeout)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

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
