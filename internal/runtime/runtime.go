package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/api"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/coldscan"
	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/comms/discord"
	"github.com/MelloB1989/karmax/internal/comms/whatsapp"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/mcp"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/internal/webhook"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
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
	cold      *coldscan.Scanner
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
	waCLIPath := "/home/mellob/code/wacli/wacli"
	waTarget := ""

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
				wacliPath = "/home/mellob/code/wacli/wacli"
			}
			targetChat := chCfg.Settings["target_chat"]
			waCLIPath = wacliPath
			waTarget = targetChat
			ch := whatsapp.New(chCfg.ID, wacliPath, targetChat, log)
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

	toolReg := tools.NewRegistry()
	registerBuiltinTools(toolReg)

	// Register new builtin tools
	toolReg.Register(&builtin.ClaudeCodeTool{Store: s, AgentID: ""})
	toolReg.Register(&builtin.CodexTool{Store: s, AgentID: ""})
	toolReg.Register(&builtin.CommsSendTool{SendFunc: commsMgr.Send})
	toolReg.Register(&builtin.GoogleWorkspaceTool{GWSPath: "/home/mellob/.local/bin/gws"})
	toolReg.Register(&builtin.GoogleWorkspaceSchemaLookupTool{GWSPath: "/home/mellob/.local/bin/gws"})
	toolReg.Register(&builtin.WhatsAppReadTool{WacliPath: waCLIPath, DefaultChat: waTarget, Store: s})
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

	memFactory := memory.NewFactory(filepath.Join(dataDir, "memory"), s, log)

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

	sched := scheduler.New(s, b, log)

	// Register scheduler tool (needs scheduler instance)
	toolReg.Register(&builtin.SchedulerTool{Scheduler: sched, AgentID: ""})

	whAddr := fmt.Sprintf("%s:%d", cfg.Webhooks.Host, cfg.Webhooks.Port)
	wh := webhook.New(whAddr, b, s, log)

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

	// Wire bus events to agent inboxes (webhooks, scheduled jobs, user-defined, and comms messages)
	sub, _ := b.Subscribe(bus.EventWebhookFired, bus.EventScheduledJob, bus.EventUserDefined, bus.EventCommsMessage)
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

	// Cold-memory background worker: summarizes older WhatsApp chats into
	// chat_summaries for the retrieval sub-agent (uses the cheaper summary model).
	coldCfg := coldscan.Config{
		Enabled:          cfg.ColdScan.Enabled,
		Interval:         time.Duration(cfg.ColdScan.IntervalMinutes) * time.Minute,
		PerTick:          cfg.ColdScan.PerTick,
		HotDays:          cfg.ColdScan.HotDays,
		MinGroupOwn:      cfg.ColdScan.MinGroupOwn,
		MinGroupOwnRatio: cfg.ColdScan.MinGroupOwnRatio,
		WacliPath:        cfg.ColdScan.WacliPath,
	}
	if len(cfg.Agents) > 0 {
		a := cfg.Agents[0]
		coldCfg.Provider = a.SummaryModel.Provider
		if coldCfg.Provider == "" {
			coldCfg.Provider = a.Provider
		}
		coldCfg.Model = a.SummaryModel.Model
		if coldCfg.Model == "" {
			coldCfg.Model = a.Model
		}
		for _, fb := range a.FallbackModels {
			coldCfg.Fallbacks = append(coldCfg.Fallbacks, karmahelper.FallbackModel{Provider: fb.Provider, Model: fb.Model})
		}
	}
	coldScanner := coldscan.New(coldCfg, s, log)

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
		cold:      coldScanner,
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

	go rt.cold.Start(ctx)

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
