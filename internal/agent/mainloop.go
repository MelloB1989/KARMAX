package agent

import (
	"context"
	"fmt"
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
}

// NewMainModelSession creates a new MainModelSession, loading any persisted
// chat history from the store and resuming token tracking.
func NewMainModelSession(cfg MainModelConfig, agentTools []tools.Tool, s *store.Store, agentID string, log *zap.Logger) (*MainModelSession, error) {
	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:     cfg.Provider,
		Model:        cfg.Model,
		SystemPrompt: cfg.SystemPrompt,
		Temperature:  cfg.Temperature,
		MaxTokens:    cfg.MaxTokens,
	}, agentTools)

	// Load persisted chat history.
	stored, err := s.LoadChatHistory(agentID, 0)
	if err != nil {
		return nil, fmt.Errorf("load chat history: %w", err)
	}

	var history models.AIChatHistory
	history.Messages = make([]models.AIMessage, 0, len(stored))
	for _, m := range stored {
		role := models.User
		switch m.Role {
		case "assistant":
			role = models.Assistant
		case "system":
			role = models.System
		case "tool":
			role = models.Tool
		}
		history.Messages = append(history.Messages, models.AIMessage{
			Role:    role,
			Message: m.Content,
		})
	}

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
		log:              log,
		compactThreshold: compactThreshold,
		keepRecent:       keepRecent,
	}, nil
}

// ProcessMessage sends a user message to the main model, persists the
// exchange, and returns the assistant response. Token counts are estimated
// from message length (1 token ~ 4 chars) until we wire actual provider
// token counts through karmahelper.Session.
func (m *MainModelSession) ProcessMessage(ctx context.Context, userMessage string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	response, _, err := m.session.Chat(ctx, userMessage)
	if err != nil {
		return "", fmt.Errorf("main model chat: %w", err)
	}

	// Estimate token counts (will be replaced with actual counts when
	// karmahelper exposes LastResponse token metadata).
	userTokens := len(userMessage) / 4
	if userTokens < 1 {
		userTokens = 1
	}
	assistantTokens := len(response) / 4
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

	// Persist assistant response.
	if persistErr := m.store.AppendChatMessage(store.StoredChatMessage{
		ID:      uuid.New().String(),
		AgentID: m.agentID,
		Role:    "assistant",
		Content: response,
		Tokens:  assistantTokens,
	}); persistErr != nil {
		m.log.Error("failed to persist assistant message", zap.Error(persistErr))
	}

	m.totalTokens += int64(userTokens + assistantTokens)

	return response, nil
}

// NeedsCompaction returns true when the accumulated token count has reached
// or exceeded the compaction threshold.
func (m *MainModelSession) NeedsCompaction() bool {
	return m.totalTokens >= int64(m.compactThreshold)
}

// GetHistory returns a pointer to the current in-memory chat history.
func (m *MainModelSession) GetHistory() *models.AIChatHistory {
	return &m.history
}

// SetHistory replaces the in-memory chat history (typically after compaction).
func (m *MainModelSession) SetHistory(h models.AIChatHistory) {
	m.history = h
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
