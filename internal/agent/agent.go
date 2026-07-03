package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

type Agent struct {
	def       AgentDef
	status    AgentStatus
	restarts  int
	startedAt time.Time
	lastEvent time.Time
	lastErr   error
	log       *zap.Logger
	bus       *bus.Bus
	store     *store.Store
	memory    *memory.Manager
	tools     []tools.Tool
	mcpTools  []tools.Tool
	inbox     chan bus.Event
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.RWMutex
	paused    bool

	// Multi-model architecture
	mainSession  *MainModelSession
	memoryModel  *MemoryModel
	summaryModel *SummaryModel

	// allTools is the complete, agent-bound toolset the main model runs with
	// (built-in + agent-scoped memory/profile tools + MCP). Kept so the API/CLI
	// can list and invoke exactly what the harness itself has.
	allTools []tools.Tool

	// Communication send function (injected to avoid circular imports)
	commsSend func(channelID, target, content string) error
	// Communication escalation function for permission requests and failures.
	commsEscalate func(agentID, primaryChannelID, content string) error

	// commsChannels holds channel info injected by the runtime so the agent
	// can build context about available communication channels for the LLM.
	commsChannels []CommsChannelInfo

	// operatorChats are the normalized chat identifiers that are the OPERATOR's
	// own (their command chats). Messages from these are commands to KARMAX;
	// messages from any other monitored chat are third parties the agent acts
	// for (proactive proxy mode).
	operatorChats map[string]bool

	parentCtx      context.Context
	errorStreak    int
	restartPending bool
}

// CommsChannelInfo describes a communication channel available to the agent.
type CommsChannelInfo struct {
	KarmaxChannelID string // KARMAX-level channel ID (e.g., "discord-main")
	Type            string // "discord", "slack", etc.
	DND             bool
}

func NewAgent(def AgentDef, b *bus.Bus, s *store.Store, mem *memory.Manager, agentTools []tools.Tool, mcpTools []tools.Tool, log *zap.Logger) *Agent {
	return &Agent{
		def:      def,
		status:   StatusIdle,
		log:      log.With(zap.String("agent", def.ID)),
		bus:      b,
		store:    s,
		memory:   mem,
		tools:    agentTools,
		mcpTools: mcpTools,
		inbox:    make(chan bus.Event, 64),
	}
}

// SetCommsSend injects the communication send function used by the agent
// to dispatch messages via communication channels.
func (a *Agent) SetCommsSend(fn func(channelID, target, content string) error) {
	a.commsSend = fn
}

// SetCommsEscalate injects the escalation function used for dangerous actions
// or primary-channel failures.
func (a *Agent) SetCommsEscalate(fn func(agentID, primaryChannelID, content string) error) {
	a.commsEscalate = fn
}

// SetOperatorChats registers the operator's own chat identifiers (phone/JID/
// "@lid"). Messages from these are treated as commands; messages from any other
// monitored chat trigger proactive proxy handling.
func (a *Agent) SetOperatorChats(chats []string) {
	set := make(map[string]bool)
	for _, c := range chats {
		if n := normalizeChat(c); n != "" {
			set[n] = true
		}
	}
	a.operatorChats = set
}

// normalizeChat reduces a chat id/phone to comparable digits/id, stripping any
// "@domain" and ":device" suffix.
func normalizeChat(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "@:"); i >= 0 {
		s = s[:i]
	}
	return s
}

// isFromOperator reports whether a chat id belongs to the operator (a command
// chat). Unknown/empty chats default to true (treat as operator/command) so we
// never accidentally auto-proxy an unrecognized chat.
func (a *Agent) isFromOperator(chatID string) bool {
	if len(a.operatorChats) == 0 {
		return true
	}
	n := normalizeChat(chatID)
	if n == "" {
		return true
	}
	return a.operatorChats[n]
}

// SetCommsChannels injects available channel info so the agent can build
// context about which channels are available for sending messages.
func (a *Agent) SetCommsChannels(channels []CommsChannelInfo) {
	a.commsChannels = channels
}

func (a *Agent) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.status == StatusRunning {
		a.mu.Unlock()
		return fmt.Errorf("agent %s already running", a.def.ID)
	}

	a.ctx, a.cancel = context.WithCancel(ctx)
	a.parentCtx = ctx
	a.startedAt = time.Now()
	a.status = StatusRunning
	a.paused = false
	a.errorStreak = 0
	a.restartPending = false
	a.mu.Unlock()

	// Initialize multi-model architecture
	if err := a.initModels(); err != nil {
		a.log.Error("failed to initialize models, falling back to legacy mode", zap.Error(err))
	}

	go a.run()

	a.bus.Publish(bus.NewEvent(bus.EventAgentStarted, a.def.ID, map[string]any{
		"name": a.def.Name,
	}))

	a.persistSnapshot()
	return nil
}

// initModels sets up the main, memory, and summary model sessions.
func (a *Agent) initModels() error {
	// Resolve memory model config — fall back to agent's own model/provider
	memProvider := a.def.MemoryModelCfg.Provider
	if memProvider == "" {
		memProvider = a.def.Provider
	}
	memModel := a.def.MemoryModelCfg.Model
	if memModel == "" {
		memModel = a.def.Model
	}

	// Resolve summary model config — fall back to agent's own model/provider
	sumProvider := a.def.SummaryModelCfg.Provider
	if sumProvider == "" {
		sumProvider = a.def.Provider
	}
	sumModel := a.def.SummaryModelCfg.Model
	if sumModel == "" {
		sumModel = a.def.Model
	}

	// Build the memory.retrieve tool that wraps memoryModel.Retrieve
	allTools := make([]tools.Tool, 0, len(a.tools)+len(a.mcpTools)+3)
	allTools = append(allTools, a.bindAgentTools(a.tools)...)
	allTools = append(allTools, a.mcpTools...)

	// Initialize memory model first so we can create the memory.retrieve tool
	namespace := a.def.Memory.Namespace
	if namespace == "" {
		namespace = a.def.ID
	}
	// Build fallback models from agent def (shared by all sub-models).
	var fallbackModels []karmahelper.FallbackModel
	for _, fb := range a.def.FallbackModels {
		fallbackModels = append(fallbackModels, karmahelper.FallbackModel{
			Provider: fb.Provider,
			Model:    fb.Model,
		})
	}

	a.memoryModel = NewMemoryModel(MemoryModelConfig{
		Provider:  memProvider,
		Model:     memModel,
		Namespace: namespace,
		Fallbacks: fallbackModels,
	}, a.store, a.memory, a.log)

	// Add the memory.retrieve tool wrapper
	allTools = append(allTools, a.buildMemoryRetrieveTool())

	// Add the memory.ingest tool (agent-scoped, needs the memory manager)
	allTools = append(allTools, &builtin.MemoryIngestTool{
		Store:     a.store,
		MemoryMgr: a.memory,
		AgentID:   a.def.ID,
	})

	// Add the memory.forget tool so the agent can curate/correct its own memory.
	allTools = append(allTools, &builtin.MemoryForgetTool{
		Store:     a.store,
		MemoryMgr: a.memory,
		AgentID:   a.def.ID,
	})

	// Apply the configured capacity cap for the forgetting curve.
	if a.memory != nil && a.def.Memory.MaxEntries > 0 {
		a.memory.SetMaxEntries(a.def.Memory.MaxEntries)
	}

	// Add the profile.update tool (agent-scoped) for maintaining the curated
	// ABOUT_ME.md document about the operator.
	allTools = append(allTools, &builtin.ProfileTool{
		MemoryMgr: a.memory,
		AgentID:   a.def.ID,
	})

	if a.commsEscalate != nil {
		allTools = append(allTools, &commsEscalateTool{agent: a})
	}

	// Initialize main model session
	mainCfg := MainModelConfig{
		Provider:             a.def.Provider,
		Model:                a.def.Model,
		SystemPrompt:         a.def.SystemPrompt,
		Temperature:          a.def.Temperature,
		MaxTokens:            a.def.MaxTokens,
		CompactionThreshold:  a.def.CompactionThreshold,
		CompactionKeepRecent: a.def.CompactionKeepRecent,
		FallbackModels:       fallbackModels,
	}

	mainSession, err := NewMainModelSession(mainCfg, allTools, a.store, a.def.ID, a.log)
	if err != nil {
		return fmt.Errorf("init main model session: %w", err)
	}
	a.mainSession = mainSession
	a.mu.Lock()
	a.allTools = allTools
	a.mu.Unlock()

	// Initialize summary model with same fallback models
	a.summaryModel = NewSummaryModel(SummaryModelConfig{
		Provider:       sumProvider,
		Model:          sumModel,
		FallbackModels: fallbackModels,
	}, a.log)

	a.log.Info("multi-model architecture initialized",
		zap.String("main_model", a.def.Model),
		zap.String("memory_model", memModel),
		zap.String("summary_model", sumModel),
		zap.Int("fallback_models", len(fallbackModels)),
		zap.Int64("current_tokens", a.mainSession.GetTotalTokens()),
	)

	return nil
}

// buildMemoryRetrieveTool creates a tools.Tool that wraps memoryModel.Retrieve().
func (a *Agent) buildMemoryRetrieveTool() tools.Tool {
	return &memoryRetrieveTool{agent: a}
}

func (a *Agent) bindAgentTools(in []tools.Tool) []tools.Tool {
	out := make([]tools.Tool, 0, len(in))
	for _, t := range in {
		switch tt := t.(type) {
		case *builtin.ClaudeCodeTool:
			cp := *tt
			cp.AgentID = a.def.ID
			cp.Namespace = a.def.Memory.Namespace
			out = append(out, &cp)
		case *builtin.SchedulerTool:
			cp := *tt
			cp.AgentID = a.def.ID
			out = append(out, &cp)
		case *builtin.CodexTool:
			cp := *tt
			cp.AgentID = a.def.ID
			out = append(out, &cp)
		case *builtin.ProposeTool:
			cp := *tt
			cp.AgentID = a.def.ID
			out = append(out, &cp)
		case *builtin.AppPushTool:
			cp := *tt
			cp.AgentID = a.def.ID
			out = append(out, &cp)
		case *builtin.CalendarAddTool:
			cp := *tt
			cp.AgentID = a.def.ID
			out = append(out, &cp)
		case *builtin.ReminderAddTool:
			cp := *tt
			cp.AgentID = a.def.ID
			out = append(out, &cp)
		case *builtin.ContactAddTool:
			cp := *tt
			cp.AgentID = a.def.ID
			out = append(out, &cp)
		default:
			out = append(out, t)
		}
	}
	return out
}

// memoryRetrieveTool wraps the agent's memory model as a tools.Tool.
type memoryRetrieveTool struct {
	agent *Agent
}

func (t *memoryRetrieveTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "memory.retrieve",
		Description: "Search the agent's memory and page index for relevant context. Use this to recall past conversations, decisions, facts, and project context.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "The search query to find relevant memories and context"}
			},
			"required": ["query"]
		}`),
	}
}

func (t *memoryRetrieveTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	query, _ := input["query"].(string)
	if query == "" {
		return tools.ErrorResult(fmt.Errorf("query is required")), nil
	}

	if t.agent.memoryModel == nil {
		return tools.ErrorResult(fmt.Errorf("memory model not initialized")), nil
	}

	result, err := t.agent.memoryModel.Retrieve(ctx, query)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("memory retrieval failed: %w", err)), nil
	}

	return tools.SuccessResult(map[string]any{
		"results": result,
	}), nil
}

// commsEscalateTool sends permission requests through the best available
// alternative channel when the primary channel is DND or unavailable.
type commsEscalateTool struct {
	agent *Agent
}

func (t *commsEscalateTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "comms.escalate",
		Description: "Ask the operator for permission before dangerous actions such as deleting files, force pushing, modifying production, or spending money. Uses an alternative communication channel if the primary is unavailable or in DND.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"primary_channel_id": {"type": "string", "description": "The original KARMAX channel ID, if known"},
				"action": {"type": "string", "description": "The action requiring approval"},
				"risk": {"type": "string", "description": "Why the action is dangerous or irreversible"}
			},
			"required": ["action", "risk"]
		}`),
	}
}

func (t *commsEscalateTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	if t.agent.commsEscalate == nil {
		return tools.ErrorResult(fmt.Errorf("comms escalation function not configured")), nil
	}

	primaryChannelID, _ := input["primary_channel_id"].(string)
	action, _ := input["action"].(string)
	risk, _ := input["risk"].(string)
	if strings.TrimSpace(action) == "" {
		return tools.ErrorResult(fmt.Errorf("action is required")), nil
	}
	if strings.TrimSpace(risk) == "" {
		return tools.ErrorResult(fmt.Errorf("risk is required")), nil
	}

	message := fmt.Sprintf("Permission required before action:\n\nAction: %s\nRisk: %s\n\nReply with approval before KARMAX proceeds.", action, risk)
	if err := t.agent.commsEscalate(t.agent.def.ID, primaryChannelID, message); err != nil {
		return tools.ErrorResult(fmt.Errorf("failed to send escalation request: %w", err)), nil
	}

	return tools.SuccessResult(map[string]any{
		"status":             "permission_requested",
		"primary_channel_id": primaryChannelID,
	}), nil
}

// buildProfileContext injects the curated ABOUT_ME profile so the agent always
// knows who the operator is from the profile (never a hardcoded identity).
func (a *Agent) buildProfileContext() string {
	if a.memory == nil {
		return ""
	}
	p, err := a.memory.ReadProfile()
	if err != nil || strings.TrimSpace(p) == "" {
		return ""
	}
	if len(p) > 8000 {
		p = p[:8000] + "\n…(truncated)"
	}
	return "## Operator profile (ABOUT_ME — this is who you serve)\n\n" + p + "\n\n"
}

// buildSessionContext queries coding sessions and formats them as context.
func (a *Agent) buildSessionContext() string {
	sessions, err := a.store.ListCodingSessions(a.def.ID)
	if err != nil {
		a.log.Warn("failed to load coding sessions for context", zap.Error(err))
		return ""
	}

	if len(sessions) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Active Coding Sessions\n\n")
	sb.WriteString("These are prior coding tasks you delegated. To CONTINUE any of them (a follow-up on the same work), call claude_code.call with its exact `session_id` — do NOT start a fresh session for work that matches one of these.\n\n")

	// Show the 12 most recent sessions so a follow-up days later can still match.
	limit := 12
	if len(sessions) < limit {
		limit = len(sessions)
	}

	for _, s := range sessions[:limit] {
		sb.WriteString(fmt.Sprintf("- **[%s]** %s (session_id: %s, status: %s, last active: %s)\n",
			s.ToolType, s.Description, s.SessionID, s.Status, s.UpdatedAt.Format("2006-01-02")))
	}
	sb.WriteString("\n")

	return sb.String()
}

// buildCommsContext formats available communication channels as context for the LLM.
func (a *Agent) buildCommsContext() string {
	if len(a.commsChannels) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Available Communication Channels\n\n")
	sb.WriteString("Use the `comms.send` tool to send messages through these channels. For dangerous actions that require permission, use `comms.escalate` first.\n\n")

	for _, ch := range a.commsChannels {
		dnd := ""
		if ch.DND {
			dnd = " DND"
		}
		sb.WriteString(fmt.Sprintf("- **%s** (type: %s%s) — use channel_id: `%s`\n",
			ch.KarmaxChannelID, ch.Type, dnd, ch.KarmaxChannelID))
	}
	sb.WriteString("\n")

	return sb.String()
}

func (a *Agent) Stop() error {
	a.mu.Lock()
	if a.status != StatusRunning && a.status != StatusPaused {
		a.mu.Unlock()
		return fmt.Errorf("agent %s not running (status: %s)", a.def.ID, a.status)
	}
	a.status = StatusStopping
	a.mu.Unlock()

	a.cancel()

	// Wait briefly for goroutine to exit
	time.Sleep(100 * time.Millisecond)

	a.mu.Lock()
	a.status = StatusStopped
	a.mu.Unlock()

	a.bus.Publish(bus.NewEvent(bus.EventAgentStopped, a.def.ID, nil))
	a.persistSnapshot()
	return nil
}

func (a *Agent) Pause() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.status != StatusRunning {
		return fmt.Errorf("agent %s not running", a.def.ID)
	}
	a.paused = true
	a.status = StatusPaused
	return nil
}

func (a *Agent) Resume() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.status != StatusPaused {
		return fmt.Errorf("agent %s not paused", a.def.ID)
	}
	a.paused = false
	a.status = StatusRunning
	return nil
}

func (a *Agent) Send(e bus.Event) {
	select {
	case a.inbox <- e:
	default:
		a.log.Warn("agent inbox full, dropping event", zap.String("event_kind", string(e.Kind)))
	}
}

func (a *Agent) Snapshot() AgentSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()

	snap := AgentSnapshot{
		ID:       a.def.ID,
		Name:     a.def.Name,
		Status:   a.status,
		Restarts: a.restarts,
		Def:      a.def,
	}

	if !a.startedAt.IsZero() {
		snap.StartedAt = a.startedAt.Format(time.RFC3339)
		snap.Uptime = time.Since(a.startedAt).Truncate(time.Second).String()
	}
	if !a.lastEvent.IsZero() {
		snap.LastEvent = a.lastEvent.Format(time.RFC3339)
	}
	if a.lastErr != nil {
		snap.LastErr = a.lastErr.Error()
	}

	return snap
}

func (a *Agent) Status() AgentStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.status
}

func (a *Agent) Def() AgentDef {
	return a.def
}

func (a *Agent) run() {
	defer func() {
		if r := recover(); r != nil {
			a.mu.Lock()
			a.status = StatusCrashed
			a.lastErr = fmt.Errorf("panic: %v", r)
			a.mu.Unlock()
			a.bus.Publish(bus.NewEvent(bus.EventAgentFailed, a.def.ID, map[string]any{
				"error": fmt.Sprintf("%v", r),
			}))
		}
	}()

	a.log.Info("agent started", zap.String("model", a.def.Model), zap.String("provider", a.def.Provider))

	for {
		select {
		case <-a.ctx.Done():
			return

		case evt, ok := <-a.inbox:
			if !ok {
				return
			}

			a.mu.RLock()
			paused := a.paused
			a.mu.RUnlock()

			if paused {
				go func() {
					time.Sleep(500 * time.Millisecond)
					a.inbox <- evt
				}()
				continue
			}

			a.mu.Lock()
			a.lastEvent = time.Now()
			a.mu.Unlock()

			if err := a.handleEvent(evt); err != nil {
				streak := a.recordEventError(err)
				a.log.Error("event handling failed", zap.Error(err))
				a.bus.Publish(bus.NewEvent(bus.EventAgentFailed, a.def.ID, map[string]any{
					"error":              err.Error(),
					"consecutive_errors": streak,
				}))
				if streak >= 3 {
					a.publishCritical("agent error streak reached restart threshold", map[string]any{
						"error":              err.Error(),
						"consecutive_errors": streak,
					})
					a.triggerCleanRestart(err)
				}
				continue
			}
			a.resetEventErrors()
		}
	}
}

func (a *Agent) handleEvent(evt bus.Event) error {
	// Harness loops (e.g. tech-news) run their prompt directly through a coding
	// harness and ingest the result, bypassing the main model entirely — so they
	// keep working even when the main model is rate-limited.
	if handled, err := a.runHarnessLoop(evt); handled {
		return err
	}

	// Proactive routing for WhatsApp/chat events.
	if evt.Kind == bus.EventCommsMessage && a.commsSend != nil {
		userMsg, _ := evt.Payload["content"].(string)
		chatID, _ := evt.Payload["channel_id"].(string)
		karmaxChannelID, _ := evt.Payload["karmax_channel_id"].(string)
		senderName, _ := evt.Payload["sender_name"].(string)
		if senderName == "" {
			senderName, _ = evt.Payload["chat_name"].(string)
		}
		isGroup, _ := evt.Payload["is_group"].(bool)

		if !a.isFromOperator(chatID) {
			// PROACTIVE PROXY: a monitored chat (DM or group) had a message. Skip
			// only trivial acks (save tokens). Groups ARE handled — just more
			// conservatively (the proxy replies only when the operator is
			// actually addressed, and flags important updates otherwise).
			if karmaxChannelID == "" || isTrivialMessage(userMsg) {
				a.ingestInteractionMemory(a.ctx, evt, userMsg, "[monitored: no action]")
				return nil
			}
			a.log.Info("proactive proxy for monitored chat",
				zap.String("chat", chatID), zap.String("from", senderName), zap.Bool("group", isGroup))
			a.delegateProxyTask(karmaxChannelID, chatID, senderName, userMsg, isGroup)
			a.ingestInteractionMemory(a.ctx, evt, userMsg, "[proxy handling]")
			return nil
		}

		// OPERATOR COMMAND: flows to the model below. KARMAX itself decides how to
		// handle it — its own tools for things it can do directly, or
		// claude_code.call when IT judges the task complex/multi-step. No
		// hardcoded routing: the intelligence is the model's, not a keyword list.
	}

	recentMem, _ := a.memory.Recent(20)

	// Build user prompt from event and context
	userPrompt := buildPromptFromEvent(evt, recentMem)

	// Inject coding session context
	sessionCtx := a.buildSessionContext()

	// Inject comms context (available channels)
	commsCtx := a.buildCommsContext()

	// Retrieve relevant long-term context before complex requests.
	retrievedCtx := a.buildProactiveMemoryContext(a.ctx, evt, userPrompt)

	// Combine dynamic context and inject into the main session
	dynamicCtx := a.buildProfileContext() + sessionCtx + commsCtx + retrievedCtx
	if dynamicCtx != "" && a.mainSession != nil {
		a.mainSession.SetContext(dynamicCtx)
	} else if dynamicCtx != "" {
		userPrompt = dynamicCtx + userPrompt
	}

	// Use multi-model session if available; otherwise fall back to legacy
	if a.mainSession != nil {
		// Acknowledge slow turns: if we're still thinking after a few seconds,
		// send a lightweight "on it" to the originating chat so the operator
		// knows the message landed and isn't dropped (high-effort turns take a
		// while). Cancelled the moment the turn finishes.
		ackCancel := a.startAckWatchdog(evt)
		response, toolCalls, err := a.mainSession.ProcessMessage(a.ctx, userPrompt)
		ackCancel()
		if err != nil {
			return fmt.Errorf("main model: %w", err)
		}
		response = cleanOutboundResponse(response)

		// Log response length for diagnostics
		a.log.Debug("LLM response received",
			zap.Int("response_len", len(response)),
			zap.String("event_kind", string(evt.Kind)),
		)

		sentViaComms := false
		for _, tc := range toolCalls {
			canonical := tools.CanonicalName(tc.Name)
			if canonical == "comms_send" {
				sentViaComms = true
			}
			a.bus.Publish(bus.NewEvent(bus.EventToolCalled, a.def.ID, map[string]any{
				"tool":  canonical,
				"input": tc.Input,
			}))
		}

		// Delivery plumbing (not routing): if the model produced a reply but
		// didn't send it via comms.send, deliver its final response to the
		// originating chat so the operator isn't ghosted. KARMAX decided what to
		// do during the turn (its own tools and/or claude_code.call); this only
		// ensures its answer reaches the chat.
		if evt.Kind == bus.EventCommsMessage && a.commsSend != nil && !sentViaComms {
			karmaxChannelID, _ := evt.Payload["karmax_channel_id"].(string)
			target, _ := evt.Payload["channel_id"].(string)
			reply := strings.TrimSpace(response)
			if karmaxChannelID != "" && reply != "" {
				if err := a.commsSend(karmaxChannelID, target, reply); err != nil {
					a.log.Warn("fallback auto-reply failed",
						zap.String("channel", karmaxChannelID), zap.Error(err))
				} else {
					a.log.Info("delivered reply (model did not call comms.send)",
						zap.String("channel", karmaxChannelID), zap.Int("len", len(reply)))
				}
			}
		}

		// Check if compaction is needed
		if a.mainSession.NeedsCompaction() && a.summaryModel != nil {
			a.log.Info("compaction threshold reached, compacting chat history",
				zap.Int64("total_tokens", a.mainSession.GetTotalTokens()),
			)

			history := a.mainSession.GetHistory()
			compacted, err := a.summaryModel.Compact(
				a.ctx,
				history,
				a.mainSession.GetKeepRecent(),
				a.store,
				a.def.ID,
				a.memory,
			)
			if err != nil {
				a.log.Error("compaction failed", zap.Error(err))
			} else {
				a.mainSession.SetHistory(*compacted)
				a.mainSession.ResetTokenCount()
				a.log.Info("chat history compacted successfully")
			}
		}

		a.ingestInteractionMemory(a.ctx, evt, userPrompt, response)

		// Publish response event
		payload := map[string]any{
			"response":      response,
			"trigger_event": evt.ID,
		}
		// Pass through comms origin info so the runtime backup router can also send if needed
		if evt.Kind == "comms.message" {
			payload["channel_id"] = evt.Payload["channel_id"]
			payload["karmax_channel_id"] = evt.Payload["karmax_channel_id"]
		}
		a.bus.Publish(bus.NewEvent(bus.EventAgentMessage, a.def.ID, payload))

		a.persistSnapshot()
		return nil
	}

	// Legacy fallback: this path is only used if initModels() failed
	return a.handleEventLegacy(evt, userPrompt)
}

// startAckWatchdog sends a lightweight acknowledgement to the originating chat
// if the current turn is still running after a short delay, so the operator
// sees the message was received even when reasoning takes a while. Returns a
// cancel func to call when the turn completes. No-op for non-chat events.
func (a *Agent) startAckWatchdog(evt bus.Event) func() {
	if evt.Kind != bus.EventCommsMessage || a.commsSend == nil {
		return func() {}
	}
	channelID, _ := evt.Payload["karmax_channel_id"].(string)
	target, _ := evt.Payload["channel_id"].(string)
	if channelID == "" {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			if err := a.commsSend(channelID, target, "👀 on it…"); err != nil {
				a.log.Warn("ack watchdog send failed", zap.Error(err))
			} else {
				a.log.Info("sent slow-turn ack", zap.String("channel", channelID))
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// delegateProxyTask runs proactive-assistant handling for a message from a
// monitored third party: claude_code reads the chat context and either acts on
// the operator's behalf (routine replies/scheduling) or flags a decision for the
// operator's approval. The operator is always informed of the outcome.
func (a *Agent) delegateProxyTask(channelID, chatID, senderName, incomingMsg string, isGroup bool) {
	who := senderName
	if who == "" {
		who = chatID
	}
	go func() {
		cc := &builtin.ClaudeCodeTool{Store: a.store, AgentID: a.def.ID, Namespace: a.def.Memory.Namespace}
		wacli := hostpaths.Wacli()

		operator := "the operator"
		if len(a.operatorChats) > 0 {
			ids := make([]string, 0, len(a.operatorChats))
			for id := range a.operatorChats {
				ids = append(ids, id)
			}
			operator = "the operator (their own numbers/JIDs: " + strings.Join(ids, ", ") + ")"
		}

		context := "A monitored 1:1 chat just messaged " + operator + "."
		policy := "   - If a reply/action is ROUTINE and you're confident how the operator would respond (acknowledgements, simple scheduling, sharing already-known info, confirming availability), DO IT NOW: send with `" + wacli + " send --to " + chatID + " --text \"...\"` in the operator's natural human voice (concise; never say you're an AI/assistant). Use the `gws` CLI for calendar/email if clearly asked.\n" +
			"   - If it involves a real DECISION, a commitment, money, or anything sensitive/ambiguous, or you're not confident → DO NOT send anything; flag it.\n"
		if isGroup {
			context = "A monitored GROUP chat just had a new message. " + operator + " is a member."
			policy = "   - This is a GROUP. Only SEND a reply if the operator is directly addressed — @-mentioned, or asked a question they clearly must answer. Reply via `" + wacli + " send --to " + chatID + " --text \"...\"` in the operator's casual voice, and only for genuinely routine/known answers.\n" +
				"   - Do NOT reply to general group discussion or messages meant for other members.\n" +
				"   - If the message is a meaningful update on an active project/deal/commitment (e.g. a client saying they'll get back, a deadline, a decision) but needs no reply, treat it as APPROVE so the operator sees it — do not silently skip important client/deal activity.\n" +
				"   - Only truly irrelevant chatter is SKIP.\n"
		}

		prompt := "You are the proactive WhatsApp assistant managing the operator's WhatsApp account via the wacli CLI. " + context + "\n\n" +
			"Chat: " + who + "\n" +
			"Chat id: " + chatID + "\n" +
			"Latest message: " + incomingMsg + "\n\n" +
			"Steps:\n" +
			"1. Read recent context: run `" + wacli + " messages --chat " + chatID + " --limit 15` (newest last). If it's already handled/answered and nothing new is needed, do nothing.\n" +
			"2. Decide on the operator's behalf:\n" + policy +
			"3. Output EXACTLY one line, beginning with one of:\n" +
			"   ACTED: <one line on what you sent/did>\n" +
			"   APPROVE: <what it is + your suggested reply/action, for the operator>\n" +
			"   SKIP: <why nothing was needed>"
		res, err := cc.Execute(a.ctx, map[string]any{"prompt": prompt, "ephemeral": true})
		if err != nil || res.IsError {
			// Never fail silently: the operator must know a monitored message
			// went unhandled (especially while they sleep).
			a.log.Warn("proxy delegation failed", zap.String("chat", chatID), zap.Error(err))
			builtin.PushAppNotification(a.store, a.def.ID, "alert",
				"⚠️ Couldn't handle — "+who,
				"A monitored message needs you (assistant failed): "+truncateForLog(incomingMsg, 200))
			return
		}
		a.reportProxyOutcome(who, claudeResultSummary(res))
	}()
}

// reportProxyOutcome tells the operator what the proactive assistant did or what
// it needs a decision on. SKIP outcomes are silent (nothing worth surfacing).
func (a *Agent) reportProxyOutcome(who, outcome string) {
	outcome = strings.TrimSpace(outcome)
	if outcome == "" {
		return
	}
	if looksLikeHarnessError(outcome) {
		// e.g. "You've hit your session limit" — a failure, not a result.
		builtin.PushAppNotification(a.store, a.def.ID, "alert",
			"⚠️ Couldn't handle — "+who, "Assistant unavailable ("+truncateForLog(outcome, 120)+"); the message is unhandled.")
		return
	}
	upper := strings.ToUpper(outcome)
	switch {
	case strings.HasPrefix(upper, "SKIP"):
		a.log.Info("proxy: nothing needed", zap.String("chat", who))
	case strings.HasPrefix(upper, "APPROVE"):
		// A real decision goes to the APPROVALS inbox (actionable: approve →
		// execute), not the notification feed. Fall back to a notification only
		// if the proposal can't be written, so it is never silently lost.
		detail := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(outcome[len("APPROVE"):]), ":"))
		if detail == "" {
			detail = outcome
		}
		if _, err := builtin.CreateProposal(a.store, a.def.ID, "message",
			"Decision — "+who, "The proactive WhatsApp assistant flagged this while handling a monitored chat.", detail, "normal"); err != nil {
			a.log.Warn("proxy: proposal create failed, falling back to notification", zap.Error(err))
			builtin.PushAppNotification(a.store, a.def.ID, "alert", "🤔 Needs your decision — "+who, outcome)
		}
	default: // ACTED or freeform
		builtin.PushAppNotification(a.store, a.def.ID, "update", "✅ Handled — "+who, outcome)
	}
}

// looksLikeHarnessError detects harness output that is an error/limit message
// rather than a real result (the CLI prints these to stdout with exit 0).
func looksLikeHarnessError(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(low, "api error") ||
		strings.HasPrefix(low, "error:") ||
		strings.Contains(low, "session limit") ||
		strings.Contains(low, "usage limit") ||
		strings.Contains(low, "rate limit") ||
		strings.Contains(low, "safeguards flagged")
}

// isTrivialMessage reports whether an incoming message is too trivial to warrant
// spinning up the proactive assistant (acks, emoji, one-word replies).
func isTrivialMessage(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || len([]rune(t)) <= 3 {
		return true
	}
	switch strings.ToLower(t) {
	case "ok", "okay", "okk", "thanks", "thank you", "thx", "ty", "cool", "nice",
		"great", "done", "haha", "lol", "yep", "nope", "sure", "fine", "hmm", "hmmm":
		return true
	}
	return false
}

// claudeResultSummary extracts a concise user-facing summary from a claude_code
// tool result — its final non-empty output line (claude_code is instructed to
// end with a one-sentence summary), truncated for WhatsApp.
func claudeResultSummary(res tools.ToolResult) string {
	out, _ := res.Output.(map[string]any)
	text, _ := out["output"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			text = s
			break
		}
	}
	if len(text) > 600 {
		text = text[:600] + "…"
	}
	return text
}


// Chat processes a single synchronous user message through the agent's main
// model and returns the reply. It refreshes long-term memory on every message
// (per the operator's requirement) and runs compaction when needed. This is the
// path used by the HTTP API so the phone app can talk to the agent directly.
func (a *Agent) Chat(ctx context.Context, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("empty message")
	}

	a.mu.RLock()
	session := a.mainSession
	a.mu.RUnlock()
	if session == nil {
		return "", fmt.Errorf("agent %s is not ready", a.def.ID)
	}

	a.mu.Lock()
	a.lastEvent = time.Now()
	a.mu.Unlock()

	evt := bus.Event{Kind: "api.chat", AgentID: a.def.ID, Payload: map[string]any{"content": text}}

	// Inject the same dynamic context the event loop uses: active coding
	// sessions, available comms channels, and retrieved long-term memory.
	dynamicCtx := a.buildProfileContext() + a.buildSessionContext() + a.buildCommsContext() + a.buildProactiveMemoryContext(ctx, evt, text)
	if strings.TrimSpace(dynamicCtx) != "" {
		session.SetContext(dynamicCtx)
	}

	response, toolCalls, err := session.ProcessMessage(ctx, text)
	if err != nil {
		return "", fmt.Errorf("chat: %w", err)
	}
	response = cleanOutboundResponse(response)

	for _, tc := range toolCalls {
		a.bus.Publish(bus.NewEvent(bus.EventToolCalled, a.def.ID, map[string]any{
			"tool": tools.CanonicalName(tc.Name), "input": tc.Input,
		}))
	}

	// Keep memory updated on every interaction.
	a.ingestInteractionMemory(ctx, evt, text, response)

	if session.NeedsCompaction() && a.summaryModel != nil {
		history := session.GetHistory()
		if compacted, cerr := a.summaryModel.Compact(ctx, history, session.GetKeepRecent(), a.store, a.def.ID, a.memory); cerr == nil {
			session.SetHistory(*compacted)
			session.ResetTokenCount()
		} else {
			a.log.Warn("compaction failed during chat", zap.Error(cerr))
		}
	}

	a.bus.Publish(bus.NewEvent(bus.EventAgentMessage, a.def.ID, map[string]any{
		"response": response,
		"source":   "api.chat",
	}))
	a.persistSnapshot()

	return response, nil
}

// ToolManifests returns the manifest of every tool the agent's main model has —
// built-ins, agent-scoped (memory.*, profile.update, comms.escalate), and MCP.
// This is the parity surface exposed over the API/CLI.
func (a *Agent) ToolManifests() []tools.ToolManifest {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]tools.ToolManifest, 0, len(a.allTools))
	for _, t := range a.allTools {
		out = append(out, t.Manifest())
	}
	return out
}

// ExecuteTool invokes one of the agent's tools by name (dotted or canonical
// form) with the given input — the same call the main model would make. Used by
// the HTTP API so the CLI (and delegated harnesses shelling out to it) can do
// anything the harness itself can.
func (a *Agent) ExecuteTool(ctx context.Context, name string, input map[string]any) (tools.ToolResult, error) {
	a.mu.RLock()
	toolset := a.allTools
	a.mu.RUnlock()

	want := tools.CanonicalName(name)
	for _, t := range toolset {
		if tools.CanonicalName(t.Manifest().Name) == want {
			a.bus.Publish(bus.NewEvent(bus.EventToolCalled, a.def.ID, map[string]any{
				"tool":   t.Manifest().Name,
				"input":  input,
				"source": "api",
			}))
			return t.Execute(ctx, input)
		}
	}
	return tools.ToolResult{}, fmt.Errorf("unknown tool: %s", name)
}

func (a *Agent) buildProactiveMemoryContext(ctx context.Context, evt bus.Event, userPrompt string) string {
	if a.memoryModel == nil || !shouldRetrieveMemory(evt, userPrompt) {
		return ""
	}

	query := memoryQueryFromEvent(evt, userPrompt)
	if strings.TrimSpace(query) == "" {
		return ""
	}

	result, err := a.buildMemoryRetrieveTool().Execute(ctx, map[string]any{"query": query})
	if err != nil || result.IsError {
		if err != nil {
			a.log.Warn("proactive memory retrieval failed", zap.Error(err))
		} else {
			a.log.Warn("proactive memory retrieval failed", zap.String("error", result.Error))
		}
		return ""
	}

	output, ok := result.Output.(map[string]any)
	if !ok {
		return ""
	}
	results, _ := output["results"].(string)
	results = strings.TrimSpace(results)
	if results == "" {
		return ""
	}

	return "## Retrieved Memory Context\n\n" + results + "\n\n"
}

func shouldRetrieveMemory(evt bus.Event, userPrompt string) bool {
	if evt.Kind != bus.EventCommsMessage && evt.Kind != bus.EventUserDefined && evt.Kind != bus.EventWebhookFired {
		return false
	}

	query := strings.ToLower(memoryQueryFromEvent(evt, userPrompt))
	if len(query) > 180 {
		return true
	}

	complexTerms := []string{
		"remember", "decision", "decide", "preference", "project", "repo",
		"implement", "debug", "fix", "deploy", "production", "context",
		"why", "how", "what did we", "previous", "again",
	}
	for _, term := range complexTerms {
		if strings.Contains(query, term) {
			return true
		}
	}
	return false
}

func memoryQueryFromEvent(evt bus.Event, fallback string) string {
	if evt.Payload != nil {
		if content, _ := evt.Payload["content"].(string); strings.TrimSpace(content) != "" {
			return truncateStr(cleanOutboundResponse(content), 1000)
		}
	}
	return truncateStr(cleanOutboundResponse(fallback), 1000)
}

func (a *Agent) ingestInteractionMemory(_ context.Context, _ bus.Event, _, _ string) {
	// No-op: raw conversation turns (chat questions, the agent's replies, incoming
	// WhatsApp messages) are NOT auto-stored — that floods memory with chatter.
	// Durable facts are saved deliberately via the memory.ingest tool (by the
	// agent and the sync loops).
}

func (a *Agent) ingestMemory(ctx context.Context, content, category, importance string, tags []string) {
	if a.memory == nil {
		return
	}

	tool := &builtin.MemoryIngestTool{
		Store:     a.store,
		MemoryMgr: a.memory,
		AgentID:   a.def.ID,
	}

	tagSet := make(map[string]bool, len(tags)+1)
	cleanTags := make([]string, 0, len(tags)+1)
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || tagSet[tag] {
			continue
		}
		tagSet[tag] = true
		cleanTags = append(cleanTags, tag)
	}

	result, err := tool.Execute(ctx, map[string]any{
		"content":    truncateStr(cleanOutboundResponse(content), 4000),
		"category":   category,
		"importance": importance,
		"tags":       strings.Join(cleanTags, ","),
	})
	if err != nil {
		a.log.Warn("memory ingest execution failed", zap.Error(err))
		return
	}
	if result.IsError {
		a.log.Warn("memory ingest failed", zap.String("error", result.Error))
	}
}

func classifyMemoryCategory(content string) string {
	lower := strings.ToLower(content)
	switch {
	case strings.Contains(lower, "prefer") || strings.Contains(lower, "preference") || strings.Contains(lower, "always ") || strings.Contains(lower, "never "):
		return "preference"
	case strings.Contains(lower, "decision") || strings.Contains(lower, "decided") || strings.Contains(lower, "we will") || strings.Contains(lower, "approved"):
		return "decision"
	case strings.Contains(lower, "project") || strings.Contains(lower, "repo") || strings.Contains(lower, "workspace") || strings.Contains(lower, "module") || strings.Contains(lower, "production"):
		return "project"
	case strings.Contains(lower, "task") || strings.Contains(lower, "todo") || strings.Contains(lower, "follow up"):
		return "task"
	default:
		return "context"
	}
}

func (a *Agent) recordEventError(err error) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastErr = err
	a.errorStreak++
	return a.errorStreak
}

func (a *Agent) resetEventErrors() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errorStreak = 0
	a.lastErr = nil
}

func (a *Agent) triggerCleanRestart(reason error) {
	a.mu.Lock()
	if a.restartPending {
		a.mu.Unlock()
		return
	}
	a.restartPending = true
	parentCtx := a.parentCtx
	a.mu.Unlock()

	go func() {
		if parentCtx == nil {
			parentCtx = context.Background()
		}
		a.handleRestart(parentCtx)
		a.mu.Lock()
		a.restartPending = false
		a.errorStreak = 0
		if reason != nil {
			a.lastErr = reason
		}
		a.mu.Unlock()
	}()
}

func (a *Agent) publishCritical(message string, fields map[string]any) {
	payload := map[string]any{
		"severity": "critical",
		"message":  message,
		"agent_id": a.def.ID,
	}
	for k, v := range fields {
		payload[k] = v
	}
	a.bus.Publish(bus.NewEvent(bus.EventSystemCritical, a.def.ID, payload))
}

// handleEventLegacy is the original single-session event handler, used
// as a fallback when multi-model initialization fails.
func (a *Agent) handleEventLegacy(evt bus.Event, userPrompt string) error {
	allTools := append(a.bindAgentTools(a.tools), a.mcpTools...)

	session := karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:     a.def.Provider,
		Model:        a.def.Model,
		SystemPrompt: a.def.SystemPrompt,
		Temperature:  a.def.Temperature,
		MaxTokens:    a.def.MaxTokens,
	}, allTools)

	response, toolCalls, _, err := session.Chat(a.ctx, userPrompt)
	if err != nil {
		return fmt.Errorf("chat inference: %w", err)
	}
	response = cleanOutboundResponse(response)
	if strings.TrimSpace(response) == "" {
		return fmt.Errorf("legacy model returned empty response after retries")
	}

	for _, tc := range toolCalls {
		a.bus.Publish(bus.NewEvent(bus.EventToolCalled, a.def.ID, map[string]any{
			"tool": tools.CanonicalName(tc.Name), "input": tc.Input,
		}))
	}

	// No auto-reply — LLM handles it via comms_send tool.
	a.ingestInteractionMemory(a.ctx, evt, userPrompt, response)

	a.bus.Publish(bus.NewEvent(bus.EventAgentMessage, a.def.ID, map[string]any{
		"response":      response,
		"trigger_event": evt.ID,
		"tool_calls":    len(toolCalls),
	}))

	a.persistSnapshot()
	return nil
}

func buildPromptFromEvent(evt bus.Event, recentMem []memory.MemoryEntry) string {
	var prompt string

	if len(recentMem) > 0 {
		prompt += "## Recent Context\n\n"
		for _, m := range recentMem {
			prompt += fmt.Sprintf("[%s] %s: %s\n\n", m.CreatedAt.Format("15:04"), m.Role, truncateStr(m.Content, 500))
		}
		prompt += "---\n\n"
	}

	prompt += "## Current Task\n\n"

	// Loops and other prompt-carrying events surface their instruction directly
	// so the agent acts on the prompt rather than parsing raw event JSON.
	if loopName, p := extractLoopPrompt(evt); p != "" {
		if loopName != "" {
			prompt += fmt.Sprintf("Scheduled loop **%s** fired. Decide what to do and act on this instruction:\n\n%s\n", loopName, p)
		} else {
			prompt += p + "\n"
		}
		return prompt
	}

	if evt.Payload != nil {
		payloadJSON, _ := json.MarshalIndent(evt.Payload, "", "  ")
		prompt += fmt.Sprintf("Event: %s\nAgent: %s\n\n```json\n%s\n```\n", evt.Kind, evt.AgentID, string(payloadJSON))
	} else {
		prompt += fmt.Sprintf("Event: %s\n", evt.Kind)
	}

	return prompt
}

// extractLoopPrompt pulls a human-written instruction (and optional loop name)
// out of an event. Direct events carry "prompt" at the top level; scheduled-job
// events nest the job payload under "payload".
func extractLoopPrompt(evt bus.Event) (loopName, prompt string) {
	if evt.Payload == nil {
		return "", ""
	}
	if p, ok := evt.Payload["prompt"].(string); ok && strings.TrimSpace(p) != "" {
		ln, _ := evt.Payload["loop"].(string)
		return ln, p
	}
	if inner, ok := evt.Payload["payload"].(map[string]any); ok {
		if p, ok := inner["prompt"].(string); ok && strings.TrimSpace(p) != "" {
			ln, _ := inner["loop"].(string)
			return ln, p
		}
	}
	return "", ""
}

// runHarnessLoop handles scheduled loops that declare a `harness` (e.g.
// "claude_code"): it runs the loop prompt DIRECTLY through that coding harness
// and ingests the output to long-term memory, bypassing the main model. This
// keeps web-research loops (e.g. tech-news) working even when the main model is
// rate-limited. Returns handled=true if it took ownership of the event.
func (a *Agent) runHarnessLoop(evt bus.Event) (bool, error) {
	if evt.Kind != bus.EventScheduledJob {
		return false, nil
	}
	inner, _ := evt.Payload["payload"].(map[string]any)
	if inner == nil {
		return false, nil
	}
	harness, _ := inner["harness"].(string)
	harness = strings.TrimSpace(harness)
	if harness == "" {
		return false, nil
	}
	prompt, _ := inner["prompt"].(string)
	loopName, _ := inner["loop"].(string)
	if strings.TrimSpace(prompt) == "" {
		return true, fmt.Errorf("harness loop %q has no prompt", loopName)
	}

	want := "claude_code.call"
	if harness == "codex" {
		want = "codex.call"
	}
	var tool tools.Tool
	for _, t := range a.bindAgentTools(a.tools) {
		if tools.CanonicalName(t.Manifest().Name) == tools.CanonicalName(want) {
			tool = t
			break
		}
	}
	if tool == nil {
		return true, fmt.Errorf("harness loop %q: tool %s not available", loopName, want)
	}

	a.log.Info("running harness loop", zap.String("loop", loopName), zap.String("harness", harness))
	ctx, cancel := context.WithTimeout(a.ctx, 9*time.Minute)
	defer cancel()

	res, err := tool.Execute(ctx, map[string]any{"prompt": prompt})
	if err != nil {
		return true, fmt.Errorf("harness loop %q: %w", loopName, err)
	}
	out := harnessOutputText(res)
	if strings.TrimSpace(out) == "" {
		a.log.Warn("harness loop produced no output", zap.String("loop", loopName))
		return true, nil
	}

	if a.memory != nil {
		if werr := a.memory.Write(memory.MemoryEntry{
			Role:    "assistant",
			Content: out,
			Tags:    []string{"loop", loopName},
		}); werr != nil {
			a.log.Warn("harness loop ingest failed", zap.String("loop", loopName), zap.Error(werr))
		}
	}
	a.log.Info("harness loop done", zap.String("loop", loopName), zap.Int("chars", len(out)))
	a.persistSnapshot()
	return true, nil
}

// harnessOutputText pulls the text body out of a coding-harness tool result
// (claude_code.call / codex.call both return {"output": "..."}).
func harnessOutputText(res tools.ToolResult) string {
	if res.IsError {
		return ""
	}
	if m, ok := res.Output.(map[string]any); ok {
		if s, ok := m["output"].(string); ok {
			return s
		}
	}
	if s, ok := res.Output.(string); ok {
		return s
	}
	return ""
}

func (a *Agent) persistSnapshot() {
	snap := a.Snapshot()
	defJSON, _ := json.Marshal(snap.Def)
	a.store.SaveAgentSnapshot(store.AgentSnapshot{
		ID:       snap.ID,
		Name:     snap.Name,
		Status:   string(snap.Status),
		Restarts: snap.Restarts,
		DefJSON:  string(defJSON),
		LastErr:  snap.LastErr,
	})
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func cleanOutboundResponse(s string) string {
	return karmahelper.CleanContent(s)
}
