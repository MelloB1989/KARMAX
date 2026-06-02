package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// MemoryModel is a lightweight model session used as a tool by the main model
// to search the agent's memory and page index for relevant context.
type MemoryModel struct {
	session   *karmahelper.Session
	history   models.AIChatHistory
	store     *store.Store
	memMgr    *memory.Manager
	namespace string
	log       *zap.Logger
}

// MemoryModelConfig configures the memory retrieval model.
type MemoryModelConfig struct {
	Provider  string
	Model     string
	Namespace string
}

const memoryModelSystemPrompt = `You are a memory retrieval assistant. When given a query, search through the agent's memory and page index to find relevant context. Return structured results with relevance scores. Be thorough but concise.`

// NewMemoryModel creates a memory model that searches memory entries and
// the page index tree on behalf of the main model.
func NewMemoryModel(cfg MemoryModelConfig, s *store.Store, memMgr *memory.Manager, log *zap.Logger) *MemoryModel {
	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:     cfg.Provider,
		Model:        cfg.Model,
		SystemPrompt: memoryModelSystemPrompt,
		MaxTokens:    4096,
	}, nil)

	return &MemoryModel{
		session:   sess,
		history:   models.AIChatHistory{Messages: []models.AIMessage{}},
		store:     s,
		memMgr:    memMgr,
		namespace: cfg.Namespace,
		log:       log,
	}
}

// Retrieve searches both the memory manager and the page index for content
// matching query, then returns a formatted summary of all results.
func (mm *MemoryModel) Retrieve(ctx context.Context, query string) (string, error) {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Memory Search Results for: %s\n\n", query))

	// Search memory entries via the manager.
	memResults, err := mm.memMgr.Search(query, 10)
	if err != nil {
		mm.log.Warn("memory search failed", zap.Error(err))
		sb.WriteString("*Memory search unavailable.*\n\n")
	} else if len(memResults) == 0 {
		sb.WriteString("*No memory entries matched.*\n\n")
	} else {
		for _, r := range memResults {
			sb.WriteString(fmt.Sprintf("### [Score: %.2f] %s\n\n%s\n\n", r.Score, r.Entry.Role, r.Excerpt))
		}
	}

	// Search page index nodes.
	sb.WriteString("## PageIndex Results\n\n")
	nodes, err := mm.store.SearchPageIndexNodes(mm.namespace, query, 10)
	if err != nil {
		mm.log.Warn("page index search failed", zap.Error(err))
		sb.WriteString("*Page index search unavailable.*\n\n")
	} else if len(nodes) == 0 {
		sb.WriteString("*No page index nodes matched.*\n\n")
	} else {
		for _, n := range nodes {
			summary := n.Summary
			if summary == "" {
				summary = n.SearchText
				if len(summary) > 200 {
					summary = summary[:200] + "..."
				}
			}
			sb.WriteString(fmt.Sprintf("### %s\n\n%s\n\n", n.Title, summary))
		}
	}

	result := sb.String()

	// Track this exchange in the memory model's own history for continuity.
	mm.history.Messages = append(mm.history.Messages, models.AIMessage{
		Role:    models.User,
		Message: query,
	})
	mm.history.Messages = append(mm.history.Messages, models.AIMessage{
		Role:    models.Assistant,
		Message: result,
	})

	// Trim history to prevent unbounded growth: keep last 50 messages.
	if len(mm.history.Messages) > 100 {
		mm.history.Messages = mm.history.Messages[len(mm.history.Messages)-50:]
	}

	return result, nil
}
