package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MainModelSession holds the persistent main model session that processes
// user messages, tracks token usage, and signals when compaction is needed.
type MainModelSession struct {
	session          *karmahelper.Session
	history          models.AIChatHistory
	totalTokens      int64
	store            *store.Store
	agentID          string
	provider         string
	model            string
	log              *zap.Logger
	mu               sync.Mutex
	compactThreshold int // default 128000
	keepRecent       int // default 20
}

// MainModelConfig configures how the main model session is created.
type MainModelConfig struct {
	Provider             string
	Model                string
	SystemPrompt         string
	Temperature          float32
	MaxTokens            int
	CompactionThreshold  int
	CompactionKeepRecent int
	FallbackModels       []karmahelper.FallbackModel
}

// NewMainModelSession creates a new MainModelSession, loading any persisted
// chat history from the store and resuming token tracking.
func NewMainModelSession(cfg MainModelConfig, agentTools []tools.Tool, s *store.Store, agentID string, log *zap.Logger) (*MainModelSession, error) {
	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Kind:           "main",
		Provider:       cfg.Provider,
		Model:          cfg.Model,
		SystemPrompt:   cfg.SystemPrompt,
		Temperature:    cfg.Temperature,
		MaxTokens:      cfg.MaxTokens,
		FallbackModels: cfg.FallbackModels,
	}, agentTools)

	// Load persisted chat history.
	stored, err := s.LoadChatHistory(agentID, 0)
	if err != nil {
		return nil, fmt.Errorf("load chat history: %w", err)
	}

	var history models.AIChatHistory
	history.Messages = make([]models.AIMessage, 0, len(stored))
	for _, m := range stored {
		// Skip tool-role messages — they reference stale tool call IDs
		// from a previous API connection and cause "400 input item ID" errors.
		if m.Role == "tool" {
			continue
		}
		role := models.User
		switch m.Role {
		case "assistant":
			role = models.Assistant
		case "system":
			role = models.System
		}
		history.Messages = append(history.Messages, models.AIMessage{
			Role:    role,
			Message: karmahelper.CleanContent(m.Content),
			// Deliberately omit ToolCalls and ToolCallId to avoid
			// stale ID references from previous sessions.
		})
	}
	sess.SetHistory(history)

	// Retrieve persisted token sum.
	totalTokens, err := s.GetChatTokenCount(agentID)
	if err != nil {
		log.Warn("failed to load token count, starting at zero", zap.Error(err))
		totalTokens = 0
	}

	compactThreshold := 128000
	if cfg.CompactionThreshold > 0 {
		compactThreshold = cfg.CompactionThreshold
	}
	keepRecent := 20
	if cfg.CompactionKeepRecent > 0 {
		keepRecent = cfg.CompactionKeepRecent
	}

	return &MainModelSession{
		session:          sess,
		history:          history,
		totalTokens:      totalTokens,
		store:            s,
		agentID:          agentID,
		provider:         cfg.Provider,
		model:            cfg.Model,
		log:              log,
		compactThreshold: compactThreshold,
		keepRecent:       keepRecent,
	}, nil
}

// ProcessMessage sends a user message to the main model, persists the
// exchange, and returns the assistant response. Token counts are estimated
// from message length (1 token ~ 4 chars) until we wire actual provider
// token counts through karmahelper.Session.
func (m *MainModelSession) ProcessMessage(ctx context.Context, userMessage string) (string, []karmahelper.ToolCallRecord, error) {
	return m.ProcessMessageWithTools(ctx, userMessage, nil)
}

// ProcessMessageWithTools is ProcessMessage with tools lent for this turn.
//
// Used by WASM workflows, which provide tools that belong only on the turns
// they caused. They are not registered anywhere and do not survive the call.
func (m *MainModelSession) ProcessMessageWithTools(ctx context.Context, userMessage string, lent []tools.Tool) (string, []karmahelper.ToolCallRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	userMessage = karmahelper.CleanContent(userMessage)
	m.log.Info("calling main model",
		zap.Int("message_len", len(userMessage)),
		zap.Int("history_len", len(m.session.GetHistory().Messages)),
		zap.Int64("total_tokens_before", m.totalTokens),
		zap.Int("lent_tools", len(lent)),
	)

	response, toolCalls, tokens, err := m.session.ChatWithExtraTools(ctx, userMessage, lent)
	if err != nil {
		return "", nil, fmt.Errorf("main model chat: %w", err)
	}
	response = karmahelper.CleanContent(response)

	m.log.Info("main model response received",
		zap.Int("response_len", len(response)),
		zap.Int("input_tokens", tokens.InputTokens),
		zap.Int("output_tokens", tokens.OutputTokens),
		zap.Int("total_tokens", tokens.TotalTokens),
		zap.Int("cache_read", tokens.CacheReadTokens),
		zap.Int("cache_write", tokens.CacheWriteTokens),
		zap.Int("tool_calls", len(toolCalls)),
	)

	// Usage is recorded by the package meter (karmahelper.OnUsage), which sees
	// every session rather than the handful that remembered to file.

	if strings.TrimSpace(response) == "" {
		m.log.Warn("main model returned empty response",
			zap.Int("input_tokens", tokens.InputTokens),
			zap.Int("output_tokens", tokens.OutputTokens),
			zap.String("input_preview", truncateForLog(userMessage, 300)),
		)
		return "", nil, fmt.Errorf("main model returned empty response after retries and fallback")
	}

	// Use actual token counts from the provider when available,
	// fall back to estimation (1 token ~ 4 chars) otherwise.
	userTokens := tokens.InputTokens
	if userTokens < 1 {
		userTokens = len(userMessage) / 4
		if userTokens < 1 {
			userTokens = 1
		}
	}
	assistantTokens := tokens.OutputTokens
	if assistantTokens < 1 {
		assistantTokens = 1
	}

	// Persist user message.
	if persistErr := m.store.AppendChatMessage(store.StoredChatMessage{
		ID:      uuid.New().String(),
		AgentID: m.agentID,
		Role:    "user",
		Content: userMessage,
		Tokens:  userTokens,
	}); persistErr != nil {
		m.log.Error("failed to persist user message", zap.Error(persistErr))
	}

	// Persist assistant response, with what it did to produce it.
	if persistErr := m.store.AppendChatMessage(store.StoredChatMessage{
		ID:        uuid.New().String(),
		AgentID:   m.agentID,
		Role:      "assistant",
		Content:   response,
		Tokens:    assistantTokens,
		ToolCalls: encodeToolCalls(toolCalls),
	}); persistErr != nil {
		m.log.Error("failed to persist assistant message", zap.Error(persistErr))
	}

	m.totalTokens += int64(userTokens + assistantTokens)
	m.history = m.session.GetHistory()

	return response, toolCalls, nil
}

// maxStoredToolInput bounds one tool's recorded arguments. A file write or a
// long WhatsApp body would otherwise put the whole payload in the transcript
// twice — once as the call, once as whatever it produced.
const maxStoredToolInput = 600

// encodeToolCalls records what the turn actually ran.
//
// The column and the struct field have been here all along and nothing ever
// filled them, so the stored conversation showed replies with no trace of the
// work behind them: a turn that sent a message, wrote a file and armed a timer
// was indistinguishable from one that only talked. Reading it back, neither the
// operator nor the agent could tell what had already been done.
//
// Arguments are kept and results are not. What was attempted is what makes the
// transcript legible; results are large, often duplicated in the reply, and are
// the part that would make history expensive to carry.
func encodeToolCalls(calls []karmahelper.ToolCallRecord) string {
	if len(calls) == 0 {
		return ""
	}
	type storedCall struct {
		Name  string         `json:"name"`
		Input map[string]any `json:"input,omitempty"`
		Error string         `json:"error,omitempty"`
	}
	out := make([]storedCall, 0, len(calls))
	for _, c := range calls {
		sc := storedCall{Name: c.Name, Input: truncateToolInput(c.Input)}
		switch {
		case c.Error != nil:
			sc.Error = c.Error.Error()
		case c.Result.IsError:
			sc.Error = c.Result.Error
		}
		out = append(out, sc)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		// Never worth failing a turn over: the reply is the load-bearing part.
		return ""
	}
	return string(encoded)
}

// truncateToolInput shortens long argument values, keeping the keys — which
// tool ran against which chat or path stays readable even when the body does not.
func truncateToolInput(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		s, ok := v.(string)
		if !ok {
			out[k] = v
			continue
		}
		if runes := []rune(s); len(runes) > maxStoredToolInput {
			s = string(runes[:maxStoredToolInput]) + "…"
		}
		out[k] = s
	}
	return out
}

// NeedsCompaction returns true when the accumulated token count has reached
// or exceeded the compaction threshold.
func (m *MainModelSession) NeedsCompaction() bool {
	return m.totalTokens >= int64(m.compactThreshold)
}

// GetHistory returns a pointer to the current in-memory chat history.
func (m *MainModelSession) GetHistory() *models.AIChatHistory {
	m.history = m.session.GetHistory()
	return &m.history
}

// SetHistory replaces the in-memory chat history (typically after compaction).
func (m *MainModelSession) SetHistory(h models.AIChatHistory) {
	m.history = h
	m.session.SetHistory(h)
}

// ResetTokenCount zeroes the accumulated token count (typically after compaction).
func (m *MainModelSession) ResetTokenCount() {
	m.totalTokens = 0
}

// GetTotalTokens returns the current estimated token count.
func (m *MainModelSession) GetTotalTokens() int64 {
	return m.totalTokens
}

// GetKeepRecent returns the number of recent messages to preserve during compaction.
func (m *MainModelSession) GetKeepRecent() int {
	return m.keepRecent
}

// SetContext injects dynamic context into the session's history so the LLM
// receives it alongside the system prompt on the next call.
func (m *MainModelSession) SetContext(ctx string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.session.SetContext(ctx)
}

// ProcessMessageWithContext sets the dynamic context and sends the message
// without releasing the lock in between.
//
// Conversations run concurrently now, so a separate SetContext followed by
// ProcessMessage lets another conversation's context land in the gap — and the
// turn then runs with somebody else's chat history and profile pinned to it.
func (m *MainModelSession) ProcessMessageWithContext(ctx context.Context, dynamicContext, userMessage string) (string, []karmahelper.ToolCallRecord, error) {
	return m.ProcessMessageWithContextAndTools(ctx, dynamicContext, userMessage, nil)
}

// ProcessMessageWithContextAndTools is the full form: dynamic context and
// tools lent for this turn, both applied without releasing the lock between.
func (m *MainModelSession) ProcessMessageWithContextAndTools(ctx context.Context, dynamicContext, userMessage string, lent []tools.Tool) (string, []karmahelper.ToolCallRecord, error) {
	m.mu.Lock()
	if dynamicContext != "" {
		m.session.SetContext(dynamicContext)
	}
	m.mu.Unlock()
	return m.ProcessMessageWithTools(ctx, userMessage, lent)
}

// truncateForLog truncates a string for safe logging output.
func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
