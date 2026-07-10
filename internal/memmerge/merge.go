// Package memmerge consolidates long-term memory: an LLM finds duplicate,
// near-duplicate, and superseded entries within a category and merges each
// cluster into a single canonical fact. It's the "keep memory clean as it
// grows" pass — stronger than the write-time Jaccard dedup, which only catches
// near-identical wording and never reconciles a stale fact against its update.
package memmerge

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// Config parameterizes the merge pass.
type Config struct {
	Namespace string
	Provider  string
	Model     string
	Fallbacks []karmahelper.FallbackModel
	// MaxPerCategory caps how many entries from one category are sent to the
	// model in a single pass (keeps the prompt bounded).
	MaxPerCategory int
	// MinCategorySize skips small categories — nothing to consolidate.
	MinCategorySize int
}

// Merger runs consolidation passes over a memory namespace.
type Merger struct {
	cfg   Config
	store *store.Store
	mem   *memory.Manager
	log   *zap.Logger
}

func New(cfg Config, s *store.Store, mem *memory.Manager, log *zap.Logger) *Merger {
	if cfg.MaxPerCategory <= 0 {
		cfg.MaxPerCategory = 60
	}
	if cfg.MinCategorySize <= 0 {
		cfg.MinCategorySize = 8
	}
	return &Merger{cfg: cfg, store: s, mem: mem, log: log}
}

const mergePrompt = `You consolidate an operator's long-term memory. Below are memory entries from ONE category, each with an id. Some are DUPLICATES or NEAR-DUPLICATES (the same fact worded differently), or a STALE fact that a newer entry supersedes.

Cluster ONLY entries that genuinely say the same thing, or where one updates/supersedes another. For each cluster of 2+ entries, write the single best merged fact — keep the most recent and accurate information, drop the redundancy, stay compact and standalone. Do NOT merge entries that merely share a topic but carry distinct facts; leave every unique fact untouched.

Respond with ONLY JSON, no prose:
{"merges":[{"fact":"<merged standalone fact, NO [tag] prefix>","importance":"low|medium|high|critical","replaces":["<id>","<id>", "..."]}]}
Every "replaces" list MUST have 2+ ids drawn only from the ids shown. If nothing should be merged, respond exactly: {"merges":[]}`

type mergeResult struct {
	Merges []struct {
		Fact       string   `json:"fact"`
		Importance string   `json:"importance"`
		Replaces   []string `json:"replaces"`
	} `json:"merges"`
}

// Tick runs one consolidation pass over the single largest eligible category
// and returns how many entries were merged away (deleted). Processing one
// category per tick keeps each run cheap; successive ticks cover the rest.
func (mg *Merger) Tick(ctx context.Context) (int, error) {
	entries, err := mg.store.ListMemoryEntries(mg.cfg.Namespace, 2000)
	if err != nil {
		return 0, fmt.Errorf("list entries: %w", err)
	}

	// Group non-pinned entries by category (pinned facts are never merged).
	byCat := map[string][]store.StoredMemoryEntry{}
	for _, e := range entries {
		if e.Pinned {
			continue
		}
		cat := e.Category
		if cat == "" {
			cat = "context"
		}
		byCat[cat] = append(byCat[cat], e)
	}

	// Pick the largest category above the threshold.
	var bestCat string
	for cat, es := range byCat {
		if len(es) < mg.cfg.MinCategorySize {
			continue
		}
		if bestCat == "" || len(es) > len(byCat[bestCat]) {
			bestCat = cat
		}
	}
	if bestCat == "" {
		return 0, nil
	}

	batch := byCat[bestCat]
	// Oldest first, capped — stale facts (the merge targets) are the old ones.
	sort.Slice(batch, func(i, j int) bool { return batch[i].CreatedAt.Before(batch[j].CreatedAt) })
	if len(batch) > mg.cfg.MaxPerCategory {
		batch = batch[:mg.cfg.MaxPerCategory]
	}

	valid := make(map[string]store.StoredMemoryEntry, len(batch))
	var sb strings.Builder
	for _, e := range batch {
		valid[e.ID] = e
		sb.WriteString(fmt.Sprintf("- %s :: %s\n", e.ID, oneLine(e.Content, 240)))
	}

	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:       mg.cfg.Provider,
		Model:          mg.cfg.Model,
		SystemPrompt:   mergePrompt,
		MaxTokens:      3000,
		FallbackModels: mg.cfg.Fallbacks,
	}, nil)

	resp, _, _, err := sess.Chat(ctx, fmt.Sprintf("Category: %s\nEntries:\n%s", bestCat, sb.String()))
	if err != nil {
		return 0, fmt.Errorf("merge model: %w", err)
	}

	var res mergeResult
	if e := json.Unmarshal([]byte(extractJSONObject(resp)), &res); e != nil {
		mg.log.Warn("memory-merge: unparseable model output", zap.String("category", bestCat))
		return 0, nil
	}

	deleted := 0
	for _, m := range res.Merges {
		fact := strings.TrimSpace(m.Fact)
		// Keep only ids that are real, in this batch, and non-pinned.
		var ids []string
		seen := map[string]bool{}
		for _, id := range m.Replaces {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			if _, ok := valid[id]; ok {
				ids = append(ids, id)
				seen[id] = true
			}
		}
		if fact == "" || len(ids) < 2 {
			continue // never let the model invent ids or drop facts on <2 cluster
		}

		importance := normalizeImportance(m.Importance)
		formatted := fmt.Sprintf("[%s][%s] %s", bestCat, importance, fact)
		if err := mg.mem.Write(memory.MemoryEntry{
			Namespace:  mg.cfg.Namespace,
			Role:       "system",
			Content:    formatted,
			Tags:       []string{bestCat, "merged"},
			Category:   bestCat,
			Importance: importanceToInt(importance),
		}); err != nil {
			mg.log.Warn("memory-merge: write canonical failed", zap.Error(err))
			continue
		}
		for _, id := range ids {
			if err := mg.store.DeleteMemoryEntry(id); err == nil {
				deleted++
			}
		}
	}

	if deleted > 0 {
		mg.log.Info("memory-merge consolidated entries", zap.String("category", bestCat), zap.Int("deleted", deleted), zap.Int("clusters", len(res.Merges)))
	}
	return deleted, nil
}

func normalizeImportance(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return "critical"
	case "high":
		return "high"
	case "low":
		return "low"
	default:
		return "medium"
	}
}

func importanceToInt(s string) int {
	switch s {
	case "critical":
		return 4
	case "high":
		return 3
	case "low":
		return 1
	default:
		return 2
	}
}

func oneLine(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func extractJSONObject(s string) string {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return ""
}

var _ = time.Now // reserved for future rate-limiting between passes
