package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
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

	// Communication send function (injected to avoid circular imports)
	commsSend func(channelID, target, content string) error

	// commsChannels holds channel info injected by the runtime so the agent
	// can build context about available communication channels for the LLM.
	commsChannels []CommsChannelInfo
}

// CommsChannelInfo describes a communication channel available to the agent.
type CommsChannelInfo struct {
	KarmaxChannelID string // KARMAX-level channel ID (e.g., "discord-main")
	Type            string // "discord", "slack", etc.
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
	a.startedAt = time.Now()
	a.status = StatusRunning
	a.paused = false
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
	allTools := make([]tools.Tool, 0, len(a.tools)+len(a.mcpTools)+2)
	allTools = append(allTools, a.tools...)
	allTools = append(allTools, a.mcpTools...)

	// Initialize memory model first so we can create the memory.retrieve tool
	namespace := a.def.Memory.Namespace
	if namespace == "" {
		namespace = a.def.ID
	}
	a.memoryModel = NewMemoryModel(MemoryModelConfig{
		Provider:  memProvider,
		Model:     memModel,
		Namespace: namespace,
	}, a.store, a.memory, a.log)

	// Add the memory.retrieve tool wrapper
	allTools = append(allTools, a.buildMemoryRetrieveTool())

	// Add the memory.ingest tool (agent-scoped, needs the memory manager)
	allTools = append(allTools, &builtin.MemoryIngestTool{
		Store:     a.store,
		MemoryMgr: a.memory,
		AgentID:   a.def.ID,
	})

	// Build fallback models from agent def
	var fallbackModels []karmahelper.FallbackModel
	for _, fb := range a.def.FallbackModels {
		fallbackModels = append(fallbackModels, karmahelper.FallbackModel{
			Provider: fb.Provider,
			Model:    fb.Model,
		})
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

	// Show only the 10 most recent sessions
	limit := 10
	if len(sessions) < limit {
		limit = len(sessions)
	}

	for _, s := range sessions[:limit] {
		sb.WriteString(fmt.Sprintf("- **[%s]** %s (session: %s, status: %s)\n",
			s.ToolType, s.Description, s.SessionID, s.Status))
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
	sb.WriteString("Use the `comms.send` tool to send messages through these channels.\n\n")

	for _, ch := range a.commsChannels {
		sb.WriteString(fmt.Sprintf("- **%s** (type: %s) — use channel_id: `%s`\n",
			ch.KarmaxChannelID, ch.Type, ch.KarmaxChannelID))
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
				a.mu.Lock()
				a.lastErr = err
				a.mu.Unlock()
				a.log.Error("event handling failed", zap.Error(err))
				a.bus.Publish(bus.NewEvent(bus.EventAgentFailed, a.def.ID, map[string]any{
					"error": err.Error(),
				}))
			}
		}
	}
}

func (a *Agent) handleEvent(evt bus.Event) error {
	recentMem, _ := a.memory.Recent(20)

	// Build user prompt from event and context
	userPrompt := buildPromptFromEvent(evt, recentMem)

	// Inject coding session context
	sessionCtx := a.buildSessionContext()

	// Inject comms context (available channels)
	commsCtx := a.buildCommsContext()

	// Combine dynamic context and inject into the main session
	dynamicCtx := sessionCtx + commsCtx
	if dynamicCtx != "" && a.mainSession != nil {
		a.mainSession.SetContext(dynamicCtx)
	} else if dynamicCtx != "" {
		userPrompt = dynamicCtx + userPrompt
	}

	// Use multi-model session if available; otherwise fall back to legacy
	if a.mainSession != nil {
		response, err := a.mainSession.ProcessMessage(a.ctx, userPrompt)
		if err != nil {
			return fmt.Errorf("main model: %w", err)
		}

		// Log response length for diagnostics
		a.log.Debug("LLM response received",
			zap.Int("response_len", len(response)),
			zap.String("event_kind", string(evt.Kind)),
		)

		// Auto-reply to Discord messages without relying on the LLM to call comms.send
		if evt.Kind == "comms.message" && a.commsSend != nil {
			discordChannelID, _ := evt.Payload["channel_id"].(string)
			karmaxChannelID, _ := evt.Payload["karmax_channel_id"].(string)
			if discordChannelID != "" && karmaxChannelID != "" {
				replyContent := response
				if strings.TrimSpace(replyContent) == "" {
					a.log.Warn("LLM returned empty response for comms message, sending fallback",
						zap.String("event_id", evt.ID),
						zap.String("input_preview", truncateStr(userPrompt, 200)),
					)
					replyContent = "I received your message but couldn't generate a response. Please try again."
				}
				if sendErr := a.commsSend(karmaxChannelID, discordChannelID, replyContent); sendErr != nil {
					a.log.Error("failed to auto-reply to comms message",
						zap.String("karmax_channel_id", karmaxChannelID),
						zap.String("discord_channel_id", discordChannelID),
						zap.Error(sendErr),
					)
				} else {
					a.log.Debug("auto-replied to comms message",
						zap.String("karmax_channel_id", karmaxChannelID),
						zap.String("discord_channel_id", discordChannelID),
					)
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

		// Write to memory
		if a.memory != nil {
			a.memory.Write(memory.MemoryEntry{
				AgentID: a.def.ID,
				Role:    "user",
				Content: userPrompt,
				Tags:    []string{string(evt.Kind)},
			})
			a.memory.Write(memory.MemoryEntry{
				AgentID: a.def.ID,
				Role:    "assistant",
				Content: response,
			})
		}

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

// handleEventLegacy is the original single-session event handler, used
// as a fallback when multi-model initialization fails.
func (a *Agent) handleEventLegacy(evt bus.Event, userPrompt string) error {
	allTools := append(a.tools, a.mcpTools...)

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

	for _, tc := range toolCalls {
		a.bus.Publish(bus.NewEvent(bus.EventToolCalled, a.def.ID, map[string]any{
			"tool": tc.Name, "input": tc.Input,
		}))
	}

	if a.memory != nil {
		a.memory.Write(memory.MemoryEntry{
			AgentID: a.def.ID,
			Role:    "user",
			Content: userPrompt,
			Tags:    []string{string(evt.Kind)},
		})
		a.memory.Write(memory.MemoryEntry{
			AgentID: a.def.ID,
			Role:    "assistant",
			Content: response,
		})
	}

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

	if evt.Payload != nil {
		payloadJSON, _ := json.MarshalIndent(evt.Payload, "", "  ")
		prompt += fmt.Sprintf("Event: %s\nAgent: %s\n\n```json\n%s\n```\n", evt.Kind, evt.AgentID, string(payloadJSON))
	} else {
		prompt += fmt.Sprintf("Event: %s\n", evt.Kind)
	}

	return prompt
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
