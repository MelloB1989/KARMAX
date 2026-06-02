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

// Retrieve searches both the memory manager (using semantic multi-keyword
// search) and the page index for content matching query, then returns a
// formatted summary of all results with categories and relevance scores.
func (mm *MemoryModel) Retrieve(ctx context.Context, query string) (string, error) {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Memory Search Results for: %s\n\n", query))

	// Get total memory entry count for context.
	totalEntries, countErr := mm.memMgr.CountEntries()
	if countErr == nil {
		sb.WriteString(fmt.Sprintf("*Searching across %d memory entries.*\n\n", totalEntries))
	}

	// Search memory entries via semantic multi-keyword search.
	memResults, err := mm.memMgr.SearchSemantic(query, 10)
	if err != nil {
		mm.log.Warn("semantic memory search failed", zap.Error(err))
		sb.WriteString("*Memory search unavailable.*\n\n")
	} else if len(memResults) == 0 {
		sb.WriteString("*No memory entries matched.*\n\n")
	} else {
		// Group results by category (role).
		categories := make(map[string][]memory.SearchResult)
		for _, r := range memResults {
			cat := r.Entry.Role
			if cat == "" {
				cat = "general"
			}
			categories[cat] = append(categories[cat], r)
		}

		for cat, results := range categories {
			sb.WriteString(fmt.Sprintf("### Category: %s\n\n", cat))
			for _, r := range results {
				sb.WriteString(fmt.Sprintf("- **[Relevance: %.0f%%]** %s\n", r.Score*100, r.Excerpt))
			}
			sb.WriteString("\n")
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

// Stats returns counts of total memory entries and tree nodes for
// observability and debugging.
func (mm *MemoryModel) Stats() map[string]int {
	stats := make(map[string]int)

	totalEntries, err := mm.memMgr.CountEntries()
	if err != nil {
		mm.log.Warn("failed to count memory entries for stats", zap.Error(err))
		stats["total_entries"] = -1
	} else {
		stats["total_entries"] = totalEntries
	}

	stats["tree_nodes"] = mm.memMgr.TreeNodeCount()

	return stats
}
