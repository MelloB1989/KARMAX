package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/comms/discord"
	"github.com/MelloB1989/karmax/internal/comms/whatsapp"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/dashboard"
	"github.com/MelloB1989/karmax/internal/mcp"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/internal/webhook"
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
	dashboard *dashboard.Server
	comms     *comms.Manager
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

	for _, chCfg := range cfg.Comms.Channels {
		switch chCfg.Type {
		case "discord":
			ch := discord.New(chCfg.ID, chCfg.Token, log)
			if err := commsMgr.Register(ch, chCfg.AgentID); err != nil {
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
			ch := whatsapp.New(chCfg.ID, wacliPath, targetChat, log)
			if err := commsMgr.Register(ch, chCfg.AgentID); err != nil {
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

		agentTools, err := toolReg.ResolveForAgent(def.Tools)
		if err != nil {
			log.Warn("failed to resolve tools for agent", zap.String("agent", def.ID), zap.Error(err))
			agentTools = nil
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

		// Inject available comms channel info into the agent for context building
		agentChannels := commsMgr.GetChannelsForAgent(agentCfg.ID)
		var channelInfos []agent.CommsChannelInfo
		for _, ch := range agentChannels {
			channelInfos = append(channelInfos, agent.CommsChannelInfo{
				KarmaxChannelID: ch.ID(),
				Type:            ch.Type(),
			})
		}
		a.SetCommsChannels(channelInfos)
	}

	sched := scheduler.New(s, b, log)

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

	dashAddr := fmt.Sprintf("%s:%d", cfg.Dashboard.Host, cfg.Dashboard.Port)
	dash := dashboard.NewServer(dashAddr, agentReg, sched, wh, memFactory, toolReg, s, b, log)

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
		dashboard: dash,
		comms:     commsMgr,
	}, nil
}

func (rt *KarmaxRuntime) Start(ctx context.Context) error {
	rt.printBanner()

	if err := rt.mcpBridge.StartAll(ctx); err != nil {
		rt.log.Error("MCP bridge start error", zap.Error(err))
	}

	if err := rt.comms.StartAll(ctx); err != nil {
		rt.log.Error("comms start error", zap.Error(err))
	}

	if err := rt.scheduler.Start(ctx); err != nil {
		rt.log.Error("scheduler start error", zap.Error(err))
	}

	if err := rt.agents.StartAll(ctx); err != nil {
		rt.log.Error("agent start error", zap.Error(err))
	}

	// Start health checks for all agents
	for _, a := range rt.agents.List() {
		a.StartHealthCheck(ctx)
	}

	// Register scheduler triggers from agent definitions
	for _, agentCfg := range rt.cfg.Agents {
		for _, sched := range agentCfg.Triggers.Schedules {
			rt.scheduler.AddJob(scheduler.ScheduledJob{
				Name:    fmt.Sprintf("%s-trigger", agentCfg.ID),
				Cron:    sched.Cron,
				AgentID: agentCfg.ID,
				Payload: sched.Payload,
				Enabled: true,
			})
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

	if rt.cfg.Dashboard.Enabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rt.dashboard.Start(ctx, rt.bus); err != nil {
				errCh <- fmt.Errorf("dashboard: %w", err)
			}
		}()
	}

	<-ctx.Done()
	rt.log.Info("shutting down...")

	rt.agents.StopAll()
	rt.scheduler.Stop()
	rt.comms.StopAll()
	rt.mcpBridge.StopAll()
	rt.memory.StopAll()
	rt.webhooks.Stop()
	rt.dashboard.Stop()
	rt.store.Close()

	wg.Wait()
	return nil
}

func (rt *KarmaxRuntime) Bus() *bus.Bus          { return rt.bus }
func (rt *KarmaxRuntime) Agents() *agent.Registry { return rt.agents }
func (rt *KarmaxRuntime) Store() *store.Store     { return rt.store }

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
	if rt.cfg.Dashboard.Enabled {
		fmt.Printf("  + Dashboard        http://%s:%d\n", rt.cfg.Dashboard.Host, rt.cfg.Dashboard.Port)
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
