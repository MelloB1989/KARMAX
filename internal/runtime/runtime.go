package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/api"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/comms/discord"
	"github.com/MelloB1989/karmax/internal/comms/whatsapp"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/mcp"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/review"
	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/internal/webhook"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

type KarmaxRuntime struct {
	cfg       *config.KarmaxConfig
	log       *zap.Logger
	store     *store.Store
	bus       *bus.Bus
	memory    *memory.ManagerFactory
	tools     *tools.Registry
	mcpBridge *mcp.MCPBridge
	agents    *agent.Registry
	scheduler *scheduler.Scheduler
	webhooks  *webhook.WebhookServer
	comms     *comms.Manager
	api       *api.Server

	// loopkit runtime state (set by startLoopkitLoops)
	loopkitLoops     map[string]loopkit.Loop
	loopWebhooks     map[string]string // webhook route -> loop name
	loopDefaultAgent string
}

func New(cfg *config.KarmaxConfig, log *zap.Logger) (*KarmaxRuntime, error) {
	dataDir := cfg.Karmax.DataDir
	os.MkdirAll(dataDir, 0755)
	os.MkdirAll(filepath.Join(dataDir, "memory"), 0755)
	os.MkdirAll(filepath.Join(dataDir, "db"), 0755)

	dbPath := filepath.Join(dataDir, "db", "karmax.db")
	s, err := store.New(dbPath, log)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	b := bus.New(log)

	// Set provider env vars from config
	if p, ok := cfg.AI.Providers["anthropic"]; ok {
		if p.BaseURL != "" {
			os.Setenv("ANTHROPIC_BASE_URL", p.BaseURL)
		}
		if p.AuthToken != "" {
			os.Setenv("ANTHROPIC_AUTH_TOKEN", p.AuthToken)
		}
		if p.APIKey != "" {
			os.Setenv("ANTHROPIC_API_KEY", p.APIKey)
		}
	}

	// Set Google API key from config if present (for Gemini fallback)
	if p, ok := cfg.AI.Providers["google"]; ok {
		if p.APIKey != "" {
			os.Setenv("GOOGLE_API_KEY", p.APIKey)
		}
	}

	mcpBridge := mcp.NewBridge(log)
	for _, mcpCfg := range cfg.MCPs {
		if err := mcpBridge.AddServer(mcpCfg); err != nil {
			log.Error("failed to add MCP server", zap.String("id", mcpCfg.ID), zap.Error(err))
		}
	}

	// Create comms manager
	commsMgr := comms.NewManager(b, s, log)

	// Capture WhatsApp settings so the whatsapp.read tool can reuse them.
	waCLIPath := hostpaths.Wacli()
	waTarget := ""
	// Default agent for channel-originated notifications: the first configured
	// agent (channels can override via agent_id).
	waAgentID := ""
	if len(cfg.Agents) > 0 {
		waAgentID = cfg.Agents[0].ID
	}

	// WhatsApp is event-based: wacli pushes message events to KARMAX's webhook
	// endpoint (/comms/whatsapp, mounted below). KARMAX does NOT register or
	// scope that webhook — it's managed in wacli (via the `wacli` agent tool or
	// CLI), so no chat is hardcoded. The optional HMAC secret must match the
	// wacli webhook's --secret (set WHATSAPP_WEBHOOK_SECRET; empty = no verify).
	waWebhookSecret := os.Getenv("WHATSAPP_WEBHOOK_SECRET")
	var waChannel *whatsapp.WhatsAppChannel

	for _, chCfg := range cfg.Comms.Channels {
		switch chCfg.Type {
		case "discord":
			ch := discord.New(chCfg.ID, chCfg.Token, log)
			if err := commsMgr.RegisterWithOptions(ch, chCfg.AgentID, comms.ChannelOptions{
				DND: dndEnabled(chCfg.Settings),
			}); err != nil {
				log.Error("failed to register comms channel",
					zap.String("id", chCfg.ID),
					zap.Error(err),
				)
			}
		case "whatsapp":
			wacliPath := chCfg.Settings["wacli_path"]
			if wacliPath == "" {
				wacliPath = hostpaths.Wacli()
			}
			targetChat := chCfg.Settings["target_chat"]
			waCLIPath = wacliPath
			waTarget = targetChat
			if chCfg.AgentID != "" {
				waAgentID = chCfg.AgentID
			}
			ch := whatsapp.New(chCfg.ID, wacliPath, targetChat, waWebhookSecret, log)
			waChannel = ch
			if err := commsMgr.RegisterWithOptions(ch, chCfg.AgentID, comms.ChannelOptions{
				DND: dndEnabled(chCfg.Settings),
			}); err != nil {
				log.Error("failed to register comms channel",
					zap.String("id", chCfg.ID),
					zap.Error(err),
				)
			}
		default:
			log.Warn("unknown comms channel type", zap.String("type", chCfg.Type))
		}
	}

	// Operator identity: the operator's own chats (commands to KARMAX) vs
	// monitored third-party chats (proactive proxy). Comma-separated
	// phone/JID/@lid in WHATSAPP_OPERATOR_CHATS; falls back to WHATSAPP_TARGET.
	operatorChats := splitCSV(os.Getenv("WHATSAPP_OPERATOR_CHATS"))
	if len(operatorChats) == 0 && waTarget != "" {
		operatorChats = []string{waTarget}
	}

	// Act-and-inform: messages KARMAX sends to people OTHER than the operator
	// don't need approval, but the operator is shown every one via an app push.
	// Replies to the operator's own chats are skipped (they see those directly).
	commsMgr.RegisterOperatorTarget(waTarget)
	commsMgr.SetProactiveNotifier(func(target, content string) {
		body := content
		if len(body) > 240 {
			body = body[:240] + "…"
		}
		builtin.PushAppNotification(s, waAgentID, "update", "Sent to "+target, body)
	})

	toolReg := tools.NewRegistry()
	registerBuiltinTools(toolReg)

	// Register new builtin tools
	toolReg.Register(&builtin.ClaudeCodeTool{Store: s, AgentID: ""})
	toolReg.Register(&builtin.CodexTool{Store: s, AgentID: ""})
	toolReg.Register(&builtin.CommsSendTool{SendFunc: commsMgr.Send, DefaultChannelID: commsMgr.DefaultChannelID})
	toolReg.Register(&builtin.GoogleWorkspaceTool{GWSPath: hostpaths.GWS()})
	toolReg.Register(&builtin.GoogleWorkspaceSchemaLookupTool{GWSPath: hostpaths.GWS()})
	toolReg.Register(&builtin.WhatsAppReadTool{WacliPath: waCLIPath, DefaultChat: waTarget, Store: s})
	toolReg.Register(&builtin.WacliTool{WacliPath: waCLIPath})
	if cfg.Webhooks.Enabled {
		toolReg.Register(&builtin.WhatsAppMonitorTool{
			WacliPath:  waCLIPath,
			WebhookURL: fmt.Sprintf("http://127.0.0.1:%d/comms/whatsapp", cfg.Webhooks.Port),
			Secret:     waWebhookSecret,
			Protected:  operatorChats,
		})
	}
	// Only expose notify.push (ntfy) to the agent when a topic is actually
	// configured — otherwise the tool can only ever fail, so we don't offer it.
	if ntfyTopic := os.Getenv("NTFY_TOPIC"); ntfyTopic != "" {
		toolReg.Register(&builtin.NtfyPushTool{Server: os.Getenv("NTFY_SERVER"), Topic: ntfyTopic})
	}
	toolReg.Register(&builtin.AppPushTool{Store: s})
	toolReg.Register(&builtin.ProposeTool{Store: s})
	toolReg.Register(&builtin.CalendarAddTool{Store: s})
	toolReg.Register(&builtin.ReminderAddTool{Store: s})
	toolReg.Register(&builtin.ContactAddTool{Store: s})

	// The scheduler (and its tool) must exist BEFORE agents resolve their tool
	// lists — registering scheduler.add afterwards silently dropped it from
	// every agent's toolset.
	sched := scheduler.New(s, b, log)
	toolReg.Register(&builtin.SchedulerTool{Scheduler: sched, AgentID: ""})

	memFactory := memory.NewFactory(filepath.Join(dataDir, "memory"), s, log)

	// Memory upkeep (the forgetting curve: TTL pruning + capacity cap) is a
	// regular loop — visible, disableable, and manually triggerable — not a
	// hidden goroutine. It needs the memory managers, so the runtime registers
	// it here rather than the marketplace hosting it.
	loopkit.Register(loopkit.Loop{
		Name:        "memory-maintenance",
		Description: "Hourly forgetting pass over every memory namespace: prunes TTL-expired facts and enforces the capacity cap (least-valuable, non-pinned entries go first).",
		Schedule:    loopkit.Every("1h"),
		Run: func(ctx context.Context, k loopkit.Kit) error {
			removed := 0
			for _, m := range memFactory.Managers() {
				removed += m.Maintain()
			}
			if removed > 0 {
				k.Logf("memory-maintenance: forgot %d entries", removed)
			}
			return nil
		},
	})

	// Staleness review: the "is this still relevant?" check-ins that keep memory
	// current. Aggressive cadence, but capped + latched so it never spams. Needs
	// the store + the agent's model + the WhatsApp channel, so it's a runtime
	// loop like memory-maintenance.
	if len(cfg.Agents) > 0 {
		a0 := cfg.Agents[0]
		ns := a0.Memory.Namespace
		if ns == "" {
			ns = a0.ID
		}
		var fbs []karmahelper.FallbackModel
		for _, fb := range a0.FallbackModels {
			fbs = append(fbs, karmahelper.FallbackModel{Provider: fb.Provider, Model: fb.Model})
		}
		provider, model := a0.SummaryModel.Provider, a0.SummaryModel.Model
		if provider == "" {
			provider = a0.Provider
		}
		if model == "" {
			model = a0.Model
		}
		waChannelID, _ := commsMgr.FindChannelIDByType("whatsapp")
		reviewer := review.New(review.Config{
			Namespace: ns, AgentID: waAgentID, Provider: provider, Model: model, Fallbacks: fbs,
			WAChannelID: waChannelID, WATarget: waTarget, SendFunc: commsMgr.Send,
		}, s, log)
		loopkit.Register(loopkit.Loop{
			Name:        "memory-review",
			Description: "Finds stale, time-sensitive memories & reminders and asks the operator (app + WhatsApp) if each is still relevant — once per item, capped so it never spams.",
			Schedule:    loopkit.Every("90m"),
			Run: func(ctx context.Context, k loopkit.Kit) error {
				return reviewer.Tick(ctx)
			},
		})

		// Brain monitor: pings the actual brain the agent depends on and alerts
		// the operator (app + WhatsApp, fixed text — NOT model-composed, since a
		// dead brain can't write) the moment it goes down, and again when it
		// recovers. This is why the operator is never silently deaf again: a
		// codex-style usage-limit outage now announces itself. Latched so it
		// alerts on transitions, not every tick.
		mainProvider, mainModel := a0.Provider, a0.Model
		waChannelID2, _ := commsMgr.FindChannelIDByType("whatsapp")
		brainDown := false
		loopkit.Register(loopkit.Loop{
			Name:        "brain-monitor",
			Description: "Pings the agent's model every few minutes and alerts you (app + WhatsApp) if the brain goes down or comes back — so an LLM outage never silently deafens KARMAX.",
			Schedule:    loopkit.Every("10m"),
			Run: func(ctx context.Context, k loopkit.Kit) error {
				pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				sess := karmahelper.NewSession(karmahelper.SessionConfig{
					Provider: mainProvider, Model: mainModel, MaxTokens: 8, FallbackModels: fbs,
				}, nil)
				resp, _, _, perr := sess.Chat(pctx, "Reply with the single word OK.")
				healthy := perr == nil && strings.TrimSpace(resp) != ""
				switch {
				case !healthy && !brainDown:
					brainDown = true
					reason := "no response"
					if perr != nil {
						reason = perr.Error()
					}
					msg := fmt.Sprintf("⚠️ KARMAX brain is DOWN (model %s: %.140s). Your messages won't be answered until it recovers.", mainModel, reason)
					builtin.PushAppNotification(s, waAgentID, "alert", "⚠️ KARMAX brain is down", msg)
					if waChannelID2 != "" && waTarget != "" {
						_ = commsMgr.Send(waChannelID2, waTarget, msg)
					}
					k.Logf("brain-monitor: DOWN (%s)", reason)
				case healthy && brainDown:
					brainDown = false
					msg := "✅ KARMAX brain is back online. Resend anything I missed."
					builtin.PushAppNotification(s, waAgentID, "update", "✅ KARMAX brain recovered", msg)
					if waChannelID2 != "" && waTarget != "" {
						_ = commsMgr.Send(waChannelID2, waTarget, msg)
					}
					k.Logf("brain-monitor: recovered")
				}
				return nil
			},
		})
	}

	agentReg := agent.NewRegistry(b, s, log)

	for _, agentCfg := range cfg.Agents {
		def := configToAgentDef(agentCfg)

		var mem *memory.Manager
		if def.Memory.Enabled {
			mem = memFactory.For(def.ID, def.Memory.Namespace)
		} else {
			mem = memFactory.For(def.ID, def.ID)
		}

		agentTools, unresolved := toolReg.ResolveForAgent(def.Tools)
		// Agent-scoped tools (memory.*, comms.escalate, profile.update) are
		// injected per-agent in initModels, so they are expected to be absent
		// from the global registry. Only warn about genuinely unknown names,
		// and never drop the tools that did resolve.
		var unknownTools []string
		for _, name := range unresolved {
			if !tools.IsAgentScoped(name) {
				unknownTools = append(unknownTools, name)
			}
		}
		if len(unknownTools) > 0 {
			log.Warn("agent lists unknown tools (skipped)", zap.String("agent", def.ID), zap.Strings("tools", unknownTools))
		}

		var mcpTools []tools.Tool
		for _, mcpID := range def.MCPs {
			mt, err := mcpBridge.GetTools(mcpID)
			if err != nil {
				log.Warn("failed to get MCP tools", zap.String("agent", def.ID), zap.String("mcp", mcpID), zap.Error(err))
				continue
			}
			mcpTools = append(mcpTools, mt...)
		}

		a, err := agentReg.Register(def, mem, agentTools, mcpTools)
		if err != nil {
			log.Error("failed to register agent", zap.String("agent", def.ID), zap.Error(err))
			continue
		}

		// Wire comms send function into the agent
		a.SetCommsSend(commsMgr.Send)
		a.SetCommsEscalate(commsMgr.RequestEscalation)

		a.SetOperatorChats(operatorChats)

		// Inject available comms channel info into the agent for context building
		agentChannels := commsMgr.GetChannelsForAgent(agentCfg.ID)
		var channelInfos []agent.CommsChannelInfo
		for _, ch := range agentChannels {
			channelInfos = append(channelInfos, agent.CommsChannelInfo{
				KarmaxChannelID: ch.ID(),
				Type:            ch.Type(),
				DND:             commsMgr.ChannelDND(ch.ID()),
			})
		}
		a.SetCommsChannels(channelInfos)
	}

	whAddr := fmt.Sprintf("%s:%d", cfg.Webhooks.Host, cfg.Webhooks.Port)
	wh := webhook.New(whAddr, b, s, log)

	// Mount the WhatsApp event endpoint that wacli pushes message events to.
	if waChannel != nil {
		wh.AddHandler("/comms/whatsapp", waChannel.HandleWebhook)
	}

	for _, route := range cfg.Webhooks.Routes {
		wh.AddRoute(webhook.WebhookRoute{
			Path:            route.Path,
			Method:          route.Method,
			AgentID:         route.AgentID,
			BusEvent:        route.BusEvent,
			Secret:          route.Secret,
			SignatureHeader: route.SignatureHeader,
			Response:        route.Response,
		})
	}

	var apiSrv *api.Server
	if cfg.API.Enabled {
		apiAddr := fmt.Sprintf("%s:%d", cfg.API.Host, cfg.API.Port)
		apiSrv = api.New(apiAddr, cfg.API.Port, os.Getenv("KARMAX_API_TOKEN"), agentReg, s, sched, memFactory, cfg, log)
	}

	// Wire bus events to agent inboxes (webhooks, scheduled jobs, user-defined,
	// and comms messages). Webhook routes may remap their event to a custom
	// bus_event kind, and agents may declare extra event kinds in
	// triggers.events — subscribe to those too, or they are published and then
	// silently dropped.
	routedKinds := []bus.EventKind{bus.EventWebhookFired, bus.EventScheduledJob, bus.EventUserDefined, bus.EventCommsMessage}
	seenKinds := map[bus.EventKind]bool{}
	for _, k := range routedKinds {
		seenKinds[k] = true
	}
	for _, route := range cfg.Webhooks.Routes {
		if k := bus.EventKind(route.BusEvent); route.BusEvent != "" && !seenKinds[k] {
			seenKinds[k] = true
			routedKinds = append(routedKinds, k)
		}
	}
	for _, agentCfg := range cfg.Agents {
		for _, ev := range agentCfg.Triggers.Events {
			if k := bus.EventKind(ev); ev != "" && !seenKinds[k] {
				seenKinds[k] = true
				routedKinds = append(routedKinds, k)
			}
		}
	}
	sub, _ := b.Subscribe(routedKinds...)
	go func() {
		for evt := range sub.Ch {
			if evt.AgentID != "" {
				if a, ok := agentReg.Get(evt.AgentID); ok {
					a.Send(evt)
				}
			}
		}
	}()

	// Persist all events
	eventSub, _ := b.Subscribe()
	go func() {
		for evt := range eventSub.Ch {
			s.AppendEvent(evt.ID, string(evt.Kind), evt.AgentID, evt.Payload, evt.Meta)
		}
	}()

	return &KarmaxRuntime{
		cfg:       cfg,
		log:       log,
		store:     s,
		bus:       b,
		memory:    memFactory,
		tools:     toolReg,
		mcpBridge: mcpBridge,
		agents:    agentReg,
		scheduler: sched,
		webhooks:  wh,
		comms:     commsMgr,
		api:       apiSrv,
	}, nil
}

func (rt *KarmaxRuntime) Start(ctx context.Context) error {
	rt.printBanner()
	rt.startCriticalAlertLoop(ctx)

	if err := rt.mcpBridge.StartAll(ctx); err != nil {
		rt.log.Error("MCP bridge start error", zap.Error(err))
		rt.publishCritical("", "MCP bridge start error", map[string]any{"error": err.Error()})
	}

	if err := rt.comms.StartAll(ctx); err != nil {
		rt.log.Error("comms start error", zap.Error(err))
		rt.publishCritical("", "comms start error", map[string]any{"error": err.Error()})
	}

	if err := rt.scheduler.Start(ctx); err != nil {
		rt.log.Error("scheduler start error", zap.Error(err))
		rt.publishCritical("", "scheduler start error", map[string]any{"error": err.Error()})
	}

	if err := rt.agents.StartAll(ctx); err != nil {
		rt.log.Error("agent start error", zap.Error(err))
		rt.publishCritical("", "agent start error", map[string]any{"error": err.Error()})
	}

	// Start health checks for all agents
	for _, a := range rt.agents.List() {
		a.StartHealthCheck(ctx)
	}

	// Register scheduler triggers from agent definitions. Stable IDs prevent
	// duplicate jobs from accumulating in the store across restarts.
	for _, agentCfg := range rt.cfg.Agents {
		for i, sched := range agentCfg.Triggers.Schedules {
			rt.scheduler.AddJob(scheduler.ScheduledJob{
				ID:      fmt.Sprintf("agent:%s:sched:%d", agentCfg.ID, i),
				Name:    fmt.Sprintf("%s-trigger", agentCfg.ID),
				Cron:    sched.Cron,
				AgentID: agentCfg.ID,
				Payload: sched.Payload,
				Enabled: true,
			})
		}
	}

	// Register declarative loops: each fires its prompt to the target agent.
	for _, loop := range rt.cfg.Loops {
		if loop.Enabled != nil && !*loop.Enabled {
			continue
		}
		payload := map[string]any{
			"loop":   loop.Name,
			"prompt": loop.Prompt,
		}
		if loop.Harness != "" {
			payload["harness"] = loop.Harness
		}
		for k, v := range loop.Payload {
			if k == "loop" || k == "prompt" || k == "harness" {
				continue
			}
			payload[k] = v
		}
		if err := rt.scheduler.AddJob(scheduler.ScheduledJob{
			ID:      "loop:" + loop.Name,
			Name:    "loop:" + loop.Name,
			Cron:    loop.Cron,
			AgentID: loop.Agent,
			Payload: payload,
			Enabled: true,
		}); err != nil {
			rt.log.Error("failed to register loop", zap.String("loop", loop.Name), zap.Error(err))
		}
	}

	// Loops authored via the public loopkit SDK (third-party + installed).
	rt.startLoopkitLoops(ctx)

	// Drop persisted scheduler jobs for loops that no longer exist or are
	// disabled, so stale entries don't reload and fire as duplicates.
	rt.pruneStaleLoopJobs()

	// Let the API run any loop on demand (manual trigger) and report the live
	// loop list (the daemon's truth — includes runtime-registered loops).
	if rt.api != nil {
		rt.api.SetRunLoop(rt.RunLoopByName)
		rt.api.SetListLoops(func() []api.LoopInfo {
			out := make([]api.LoopInfo, 0, len(rt.loopkitLoops))
			for _, l := range rt.loopkitLoops {
				out = append(out, api.LoopInfo{
					Name:        l.Name,
					Description: l.Description,
					Schedule:    l.Schedule.CronExpr(),
					Webhook:     l.Webhook,
					Events:      l.Events,
				})
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
			return out
		})
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	if rt.cfg.Webhooks.Enabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rt.webhooks.Start(ctx); err != nil {
				errCh <- fmt.Errorf("webhook server: %w", err)
			}
		}()
	}

	if rt.api != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rt.api.Start(ctx); err != nil {
				errCh <- fmt.Errorf("api server: %w", err)
			}
		}()
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-errCh:
				if err == nil {
					continue
				}
				rt.log.Error("runtime component failed", zap.Error(err))
				rt.publishCritical("", "runtime component failed", map[string]any{"error": err.Error()})
			}
		}
	}()

	<-ctx.Done()
	rt.log.Info("shutting down...")

	rt.agents.StopAll()
	rt.scheduler.Stop()
	rt.comms.StopAll()
	rt.mcpBridge.StopAll()
	rt.memory.StopAll()
	rt.webhooks.Stop()
	if rt.api != nil {
		rt.api.Stop()
	}
	rt.store.Close()

	wg.Wait()
	return nil
}

func (rt *KarmaxRuntime) startCriticalAlertLoop(ctx context.Context) {
	sub, cancel := rt.bus.Subscribe(bus.EventSystemCritical)
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-sub.Ch:
				if !ok {
					return
				}
				message, _ := evt.Payload["message"].(string)
				if message == "" {
					message = "KARMAX critical system event"
				}
				if attempted, _ := evt.Payload["alternative_alert_attempted"].(bool); attempted {
					continue
				}
				primary, _ := evt.Payload["karmax_channel_id"].(string)
				if err := rt.comms.AlertAlternative(evt.AgentID, primary, "Critical KARMAX alert: "+message); err != nil {
					rt.log.Warn("failed to send critical alert through alternative channel",
						zap.String("agent_id", evt.AgentID),
						zap.String("primary_channel_id", primary),
						zap.Error(err),
					)
				}
			}
		}
	}()
}

func (rt *KarmaxRuntime) publishCritical(agentID, message string, fields map[string]any) {
	payload := map[string]any{
		"severity": "critical",
		"message":  message,
	}
	for k, v := range fields {
		payload[k] = v
	}
	rt.bus.Publish(bus.NewEvent(bus.EventSystemCritical, agentID, payload))
}

func (rt *KarmaxRuntime) printBanner() {
	agentCount := len(rt.agents.List())
	toolCount := len(rt.tools.List())
	mcpToolCounts := rt.mcpBridge.ServerToolCount()
	totalMCPTools := 0
	for _, c := range mcpToolCounts {
		totalMCPTools += c
	}
	commsCount := len(rt.comms.List())

	fmt.Println()
	fmt.Println("  karmax v0.2.0  |  data:", rt.cfg.Karmax.DataDir, " |  db: karmax.db")
	fmt.Println("  -------------------------------------------------")
	fmt.Println("  + SQLite store    (migrations applied)")
	fmt.Printf("  + MCP bridge      (%d servers)\n", len(rt.cfg.MCPs))
	fmt.Printf("  + Tool registry   (%d built-in + %d MCP tools)\n", toolCount, totalMCPTools)
	fmt.Printf("  + Memory manager  (%d namespaces)\n", len(rt.memory.List()))
	fmt.Printf("  + Comms channels  (%d channels)\n", commsCount)
	fmt.Printf("  + %d agents loaded\n", agentCount)

	for _, a := range rt.agents.List() {
		snap := a.Snapshot()
		triggers := ""
		if len(snap.Def.Triggers.Webhooks) > 0 {
			triggers += fmt.Sprintf("webhooks%v", snap.Def.Triggers.Webhooks)
		}
		if len(snap.Def.Triggers.Schedules) > 0 {
			for _, s := range snap.Def.Triggers.Schedules {
				triggers += fmt.Sprintf(" cron[%s]", s.Cron)
			}
		}
		if snap.Def.Triggers.RunOnStart {
			triggers += " run_on_start"
		}
		if triggers == "" {
			triggers = "manual"
		}
		fmt.Printf("    > %-18s [%s]   (%s)\n", snap.ID, snap.Status, triggers)
	}

	fmt.Printf("  + Scheduler        (%d jobs)\n", len(rt.scheduler.ListJobs()))
	if rt.cfg.Webhooks.Enabled {
		fmt.Printf("  + Webhook server   http://%s:%d\n", rt.cfg.Webhooks.Host, rt.cfg.Webhooks.Port)
	}
	if rt.cfg.API.Enabled {
		fmt.Printf("  + API server       http://%s:%d  (phone app)\n", rt.cfg.API.Host, rt.cfg.API.Port)
	}
	fmt.Println("  -------------------------------------------------")
	fmt.Println("  karmax is running. Press Ctrl+C to stop.")
	fmt.Println()
}

func registerBuiltinTools(reg *tools.Registry) {
	reg.Register(&builtin.HTTPTool{})
	reg.Register(&builtin.ShellTool{})
	reg.Register(&builtin.FileReadTool{})
	reg.Register(&builtin.FileWriteTool{})
	reg.Register(&builtin.FileListTool{})
	reg.Register(&builtin.EmailTool{})
	reg.Register(&builtin.NotifyTool{})
}

// splitCSV splits a comma-separated env value into trimmed non-empty parts.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func dndEnabled(settings map[string]string) bool {
	for _, key := range []string{"dnd", "do_not_disturb", "do-not-disturb"} {
		switch strings.ToLower(strings.TrimSpace(settings[key])) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func configToAgentDef(cfg config.AgentDefConfig) agent.AgentDef {
	def := agent.AgentDef{
		ID:                   cfg.ID,
		Name:                 cfg.Name,
		Description:          cfg.Description,
		Tags:                 cfg.Tags,
		SystemPrompt:         cfg.SystemPrompt,
		Model:                cfg.Model,
		Provider:             cfg.Provider,
		Temperature:          cfg.Temperature,
		MaxTokens:            cfg.MaxTokens,
		Tools:                cfg.Tools,
		MCPs:                 cfg.MCPs,
		RestartPolicy:        agent.RestartPolicy(cfg.RestartPolicy),
		MaxRestarts:          cfg.MaxRestarts,
		Env:                  cfg.Env,
		CompactionThreshold:  cfg.CompactionThreshold,
		CompactionKeepRecent: cfg.CompactionKeepRecent,
		MemoryModelCfg: agent.ModelConfig{
			Model:    cfg.MemoryModel.Model,
			Provider: cfg.MemoryModel.Provider,
		},
		SummaryModelCfg: agent.ModelConfig{
			Model:    cfg.SummaryModel.Model,
			Provider: cfg.SummaryModel.Provider,
		},
		Memory: agent.AgentMemoryConfig{
			Enabled:    cfg.Memory.Enabled,
			Namespace:  cfg.Memory.Namespace,
			MaxEntries: cfg.Memory.MaxEntries,
			Summarize:  cfg.Memory.Summarize,
		},
		HealthCheck: agent.HealthCheckConfig{
			IntervalSeconds: cfg.HealthCheck.IntervalSeconds,
			ToolName:        cfg.HealthCheck.ToolName,
			ToolInput:       cfg.HealthCheck.ToolInput,
			PingPrompt:      cfg.HealthCheck.PingPrompt,
		},
		Triggers: agent.AgentTriggers{
			Webhooks:   cfg.Triggers.Webhooks,
			Events:     cfg.Triggers.Events,
			RunOnStart: cfg.Triggers.RunOnStart,
		},
	}

	for _, s := range cfg.Triggers.Schedules {
		def.Triggers.Schedules = append(def.Triggers.Schedules, agent.ScheduleTrigger{
			Cron:    s.Cron,
			Payload: s.Payload,
		})
	}

	for _, fb := range cfg.FallbackModels {
		def.FallbackModels = append(def.FallbackModels, agent.FallbackModelDef{
			Provider: fb.Provider,
			Model:    fb.Model,
		})
	}

	return def
}
