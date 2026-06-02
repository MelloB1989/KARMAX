package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// SummaryModel compacts a main model's chat history by summarizing older
// messages into a single context block, preserving only the most recent
// messages verbatim.
type SummaryModel struct {
	session *karmahelper.Session
	log     *zap.Logger
}

// SummaryModelConfig configures the summary/compaction model.
type SummaryModelConfig struct {
	Provider       string
	Model          string
	FallbackModels []karmahelper.FallbackModel
}

const summaryModelSystemPrompt = `You are a conversation summarizer. Given a conversation history, produce a comprehensive summary that preserves all key information: decisions made, facts learned, user preferences, action items, project context, and any important details. The summary will replace the original conversation, so nothing important should be lost.`

// NewSummaryModel creates a summary model used to compact long chat histories.
func NewSummaryModel(cfg SummaryModelConfig, log *zap.Logger) *SummaryModel {
	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:       cfg.Provider,
		Model:          cfg.Model,
		SystemPrompt:   summaryModelSystemPrompt,
		MaxTokens:      8192,
		FallbackModels: cfg.FallbackModels,
	}, nil)

	return &SummaryModel{
		session: sess,
		log:     log,
	}
}

// Compact summarizes the older portion of the chat history (everything
// except the last keepRecent messages), replaces the full history with:
//   - the original system message
//   - a context block containing the summary
//   - the keepRecent most recent messages
//
// The compacted history is also persisted to the store.
func (sm *SummaryModel) Compact(ctx context.Context, history *models.AIChatHistory, keepRecent int, s *store.Store, agentID string, memMgr *memory.Manager) (*models.AIChatHistory, error) {
	if len(history.Messages) <= keepRecent {
		return history, nil
	}

	cutoff := len(history.Messages) - keepRecent
	oldMessages := history.Messages[:cutoff]
	recentMessages := history.Messages[cutoff:]

	// Serialize old messages into a readable transcript.
	var sb strings.Builder
	for _, msg := range oldMessages {
		role := roleToString(msg.Role)
		sb.WriteString(fmt.Sprintf("[%s] %s\n\n", role, msg.Message))
	}

	prompt := "Summarize the following conversation history:\n\n" + sb.String()

	summaryResponse, _, _, err := sm.session.Chat(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("summary model chat: %w", err)
	}

	sm.log.Info("compacted chat history",
		zap.Int("old_messages", len(oldMessages)),
		zap.Int("kept_recent", len(recentMessages)),
		zap.Int("summary_len", len(summaryResponse)),
	)

	// Ingest key facts from the summary into durable memory.
	if memMgr != nil {
		if err := memMgr.IngestFromSummary(summaryResponse, agentID); err != nil {
			sm.log.Warn("memory ingestion from summary failed", zap.Error(err))
		}
	}

	// Build the new compacted history.
	newHistory := &models.AIChatHistory{
		SystemMsg: history.SystemMsg,
		Context:   "## Previous Conversation Summary\n\n" + summaryResponse,
		Messages:  make([]models.AIMessage, len(recentMessages)),
	}
	copy(newHistory.Messages, recentMessages)

	// Persist the compacted history to the store.
	storedMessages := make([]store.StoredChatMessage, 0, len(newHistory.Messages)+1)

	// Store the summary as a system-role message so it is recoverable.
	storedMessages = append(storedMessages, store.StoredChatMessage{
		ID:      uuid.New().String(),
		AgentID: agentID,
		Role:    "system",
		Content: newHistory.Context,
		Tokens:  len(newHistory.Context) / 4,
	})

	for _, msg := range newHistory.Messages {
		storedMessages = append(storedMessages, store.StoredChatMessage{
			ID:      uuid.New().String(),
			AgentID: agentID,
			Role:    roleToString(msg.Role),
			Content: msg.Message,
			Tokens:  len(msg.Message) / 4,
		})
	}

	if err := s.ReplaceChatHistory(agentID, storedMessages); err != nil {
		sm.log.Error("failed to persist compacted history", zap.Error(err))
		// Non-fatal: the in-memory history is still compacted.
	}

	return newHistory, nil
}

// roleToString converts a models.AIRoles to its string representation.
func roleToString(r models.AIRoles) string {
	switch r {
	case models.User:
		return "user"
	case models.Assistant:
		return "assistant"
	case models.System:
		return "system"
	case models.Tool:
		return "tool"
	default:
		return "user"
	}
}
