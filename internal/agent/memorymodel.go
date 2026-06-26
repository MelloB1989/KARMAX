package agent

import (
	"context"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// MemoryModel is an agentic memory-retrieval sub-agent. The main orchestration
// agent calls it via the memory.retrieve tool with a plain question; this model
// then autonomously queries the memory database, the page-index tree, the cold
// chat summaries, the operator profile, and (when needed) live WhatsApp — across
// multiple tool calls — and returns a synthesized, accurate context block. The
// orchestrator never has to know HOW the context was assembled. (Mirrors the
// retrieve_context two-model pattern from the page-index design.)
type MemoryModel struct {
	cfg       MemoryModelConfig
	store     *store.Store
	memMgr    *memory.Manager
	namespace string
	log       *zap.Logger
}

// MemoryModelConfig configures the retrieval sub-agent.
type MemoryModelConfig struct {
	Provider  string
	Model     string
	Namespace string
	WacliPath string
	Fallbacks []karmahelper.FallbackModel
}

const memoryRetrieverPrompt = `You are KARMAX's memory retrieval agent. The orchestration agent will ask you for context with a question or topic. Your only job is to return the most ACCURATE, relevant context it needs — nothing more.

Tools:
- profile_read: the authoritative ABOUT_ME profile (identity, projects, people, goals). Read it first when identity/relationships matter.
- mem_search: semantic search over long-term memory. Run SEVERAL focused queries with different keywords — don't rely on one.
- mem_recent: the freshest "hot" memory (what's going on right now).
- tree_search: the structured page-index of memory.
- chat_summaries: background ("cold") summaries of older conversations.
- whatsapp.read: LIVE WhatsApp — pull the actual recent messages of a chat. Use ONLY when stored memory is insufficient or you must verify the latest state, since it is slower.

Method: make multiple queries, cross-check sources, and prefer specific facts over vague ones. Resolve conflicts in favor of the profile and the most recent entries. Then output a concise, well-structured context block (bullet points or short sections) containing ONLY facts relevant to the question, each with a brief source tag (profile / memory / chat / live). If you find nothing relevant, reply exactly: "No relevant context found." Never invent facts.`

// NewMemoryModel creates the agentic retrieval sub-agent.
func NewMemoryModel(cfg MemoryModelConfig, s *store.Store, memMgr *memory.Manager, log *zap.Logger) *MemoryModel {
	return &MemoryModel{
		cfg:       cfg,
		store:     s,
		memMgr:    memMgr,
		namespace: cfg.Namespace,
		log:       log,
	}
}

// retrievalTools is the tool set the sub-agent uses to query memory.
func (mm *MemoryModel) retrievalTools() []tools.Tool {
	return []tools.Tool{
		&profileReadTool{mem: mm.memMgr},
		&memSearchTool{mem: mm.memMgr},
		&memRecentTool{mem: mm.memMgr},
		&pageIndexTool{store: mm.store, namespace: mm.namespace},
		&chatSummaryTool{store: mm.store},
		&builtin.WhatsAppReadTool{WacliPath: mm.cfg.WacliPath, Store: mm.store},
	}
}

// Retrieve runs the sub-agent for one question and returns the synthesized
// context. A fresh session is used per call so retrieval is stateless and
// reliable (no cross-question contamination).
func (mm *MemoryModel) Retrieve(ctx context.Context, query string) (string, error) {
	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:       mm.cfg.Provider,
		Model:          mm.cfg.Model,
		SystemPrompt:   memoryRetrieverPrompt,
		MaxTokens:      4096,
		FallbackModels: mm.cfg.Fallbacks,
	}, mm.retrievalTools())

	resp, _, _, err := sess.Chat(ctx, query)
	if err != nil {
		mm.log.Warn("memory retrieval failed", zap.Error(err))
		return "", err
	}
	return resp, nil
}

// Stats returns counts of total memory entries and tree nodes for
// observability and debugging.
func (mm *MemoryModel) Stats() map[string]int {
	stats := make(map[string]int)
	totalEntries, err := mm.memMgr.CountEntries()
	if err != nil {
		stats["total_entries"] = -1
	} else {
		stats["total_entries"] = totalEntries
	}
	stats["tree_nodes"] = mm.memMgr.TreeNodeCount()
	return stats
}
