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

const mergePrompt = `You consolidate an operator's long-term memory. Below are memory entries from ONE category, each with a NUMBER. Some are DUPLICATES or NEAR-DUPLICATES (the same fact worded differently), or a STALE fact that a newer entry supersedes.

Cluster ONLY entries that genuinely say the same thing, or where one updates/supersedes another. For each cluster of 2+ entries, write the single best merged fact — keep the most recent and accurate information, drop the redundancy, stay compact and standalone. Do NOT merge entries that merely share a topic but carry distinct facts; leave every unique fact untouched.

Respond with ONLY JSON, no prose:
{"merges":[{"fact":"<merged standalone fact, NO [tag] prefix>","importance":"low|medium|high|critical","replaces":[<number>,<number>]}]}
Every "replaces" list MUST have 2+ NUMBERS taken from the list above. If nothing should be merged, respond exactly: {"merges":[]}`

type mergeResult struct {
	Merges []struct {
		Fact       string `json:"fact"`
		Importance string `json:"importance"`
		Replaces   []int  `json:"replaces"`
	} `json:"merges"`
}

// Tick runs one consolidation pass over the single largest eligible category
// and returns how many entries were merged away (deleted). Processing one
// category per tick keeps each run cheap; successive ticks cover the rest.
func (mg *Merger) Tick(ctx context.Context) (int, error) {
	// Read from where memory actually lives, not from where it used to.
	//
	// This returned immediately whenever a remote store was configured, on the
	// reasoning that GitLoom files every memory about one subject at one path
	// and therefore consolidates by construction. It does not consolidate
	// NEAR-duplicates: measured on the live store, "Cold outreach targets
	// decided July 21, 2026: SNITCH, RetainIQ, Omneky" is filed four times with
	// little more than a comma and an "and" between the versions, and
	// seventy-seven such pairs sit above 70% word overlap. Each is a separate
	// fact to every reader, which is how one commitment to Shravan became three
	// nags about the same promise.
	entries, err := mg.listEntries(ctx)
	if err != nil {
		return 0, err
	}

	// Group non-pinned entries by category (pinned facts are never merged).
	// Group by SUBJECT, not category. The category a survey reports is the coarse
	// tier ("facts" for nearly everything), so grouping by it made one giant
	// bucket the pass could only sample. The duplicates that matter are the same
	// subject scattered across folders by the store's own filing — nineteen
	// "Shravan collaboration dropped" facts under decisions/, people/, and
	// pending-task/. subjectKey pulls them back together regardless of folder,
	// which is the only grouping that puts a real duplicate cluster in one
	// batch.
	bySubject := map[string][]store.StoredMemoryEntry{}
	for _, e := range entries {
		if e.Pinned {
			continue
		}
		bySubject[subjectKey(e)] = append(bySubject[subjectKey(e)], e)
	}

	mg.log.Info("memory-merge: surveyed", zap.Int("entries", len(entries)), zap.Int("subjects", len(bySubject)))

	// The subject with the most entries above the threshold — the biggest pile
	// of near-duplicates, which is where a single pass removes the most.
	var bestKey string
	for k, es := range bySubject {
		if len(es) < mg.cfg.MinCategorySize {
			continue
		}
		if bestKey == "" || len(es) > len(bySubject[bestKey]) {
			bestKey = k
		}
	}
	if bestKey == "" {
		mg.log.Info("memory-merge: no subject has enough duplicates to consolidate",
			zap.Int("min", mg.cfg.MinCategorySize))
		return 0, nil
	}
	mg.log.Info("memory-merge: consolidating subject", zap.String("subject", bestKey), zap.Int("entries", len(bySubject[bestKey])))

	batch := bySubject[bestKey]
	// The merged fact keeps the cluster's own category, not the subject key —
	// the key is a filename token, the category decides which tier it lands in.
	batchCat := modalCategory(batch)
	// Oldest first, capped — stale facts (the merge targets) are the old ones.
	sort.Slice(batch, func(i, j int) bool { return batch[i].CreatedAt.Before(batch[j].CreatedAt) })
	if len(batch) > mg.cfg.MaxPerCategory {
		batch = batch[:mg.cfg.MaxPerCategory]
	}

	// Numbered, not identified.
	//
	// The entries were listed by their store id and the model was asked to
	// echo them back. With a local table those were UUIDs and it abbreviated
	// them; with GitLoom they are paths and it invented hex strings outright —
	// either way not one of eleven proposed clusters could be matched to a real
	// entry, and the pass reported success having changed nothing. Asking a
	// model to transcribe long opaque identifiers is the mistake; a number it
	// cannot get wrong is the fix.
	valid := make(map[int]store.StoredMemoryEntry, len(batch))
	var sb strings.Builder
	for i, e := range batch {
		n := i + 1
		valid[n] = e
		sb.WriteString(fmt.Sprintf("%d. %s\n", n, oneLine(e.Content, 240)))
	}

	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Kind:         "memory-merge",
		Provider:     mg.cfg.Provider,
		Model:        mg.cfg.Model,
		SystemPrompt: mergePrompt,
		// Room to finish the JSON. Sixty entries can yield a dozen clusters,
		// each carrying a rewritten fact and its ids; at 3000 the reply was cut
		// off mid-structure and the whole pass was thrown away as unparseable —
		// a model call paid for and nothing to show, intermittently, depending
		// on how much there was to merge.
		MaxTokens:      8000,
		FallbackModels: mg.cfg.Fallbacks,
	}, nil)

	resp, _, _, err := sess.Chat(ctx, fmt.Sprintf("Subject: %s\nEntries:\n%s", bestKey, sb.String()))
	if err != nil {
		return 0, fmt.Errorf("merge model: %w", err)
	}

	var res mergeResult
	if e := json.Unmarshal([]byte(extractJSONObject(resp)), &res); e != nil {
		mg.log.Warn("memory-merge: unparseable model output", zap.String("subject", bestKey))
		return 0, nil
	}

	deleted := 0
	for _, m := range res.Merges {
		fact := strings.TrimSpace(m.Fact)
		// Keep only ids that are real, in this batch, and non-pinned.
		var ids []string
		seen := map[string]bool{}
		for _, n := range m.Replaces {
			e, ok := valid[n]
			if !ok || e.ID == "" || seen[e.ID] {
				continue
			}
			ids = append(ids, e.ID)
			seen[e.ID] = true
		}
		if fact == "" || len(ids) < 2 {
			// Logged, because "the model proposed ten clusters and none applied"
			// is otherwise indistinguishable from "it found nothing".
			mg.log.Info("memory-merge: cluster rejected",
				zap.Int("ids_returned", len(m.Replaces)), zap.Int("ids_matched", len(ids)),
				zap.Ints("numbers_returned", m.Replaces),
				zap.String("fact", oneLine(fact, 60)))
			continue // never let the model invent ids or drop facts on <2 cluster
		}

		// Remove the originals FIRST. Writing the canonical fact and then failing
		// to delete what it replaces does not consolidate anything — it adds a
		// memory and keeps every duplicate, so the pass makes the store worse
		// each time it runs. Observed live: four clusters proposed, one
		// canonical fact written per cluster, zero deletions, because every id
		// was a section path the delete refused. Nothing is written unless the
		// duplicates are actually gone.
		removed := make([]string, 0, len(ids))
		for _, id := range ids {
			if err := mg.mem.Forget(id); err != nil {
				mg.log.Warn("memory-merge: could not remove a merged entry",
					zap.String("id", id), zap.Error(err))
				continue
			}
			removed = append(removed, id)
		}
		if len(removed) < 2 {
			// Fewer than two originals actually went away, so there is nothing
			// this fact would be consolidating.
			mg.log.Warn("memory-merge: skipping a cluster whose originals could not be removed",
				zap.Int("wanted", len(ids)), zap.Int("removed", len(removed)))
			continue
		}
		deleted += len(removed)

		importance := normalizeImportance(m.Importance)
		formatted := fmt.Sprintf("[%s][%s] %s", batchCat, importance, fact)
		if err := mg.mem.Write(memory.MemoryEntry{
			Namespace:  mg.cfg.Namespace,
			Role:       "system",
			Content:    formatted,
			Tags:       []string{subjectTag(bestKey), "merged"},
			Category:   batchCat,
			Importance: importanceToInt(importance),
		}); err != nil {
			mg.log.Error("memory-merge: the originals were removed but the consolidated fact could not be written — those facts are LOST",
				zap.Strings("removed", removed), zap.String("fact", oneLine(fact, 200)), zap.Error(err))
			continue
		}
	}

	if deleted > 0 {
		mg.log.Info("memory-merge consolidated entries", zap.String("subject", bestKey), zap.Int("deleted", deleted), zap.Int("clusters", len(res.Merges)))
	} else {
		// Said out loud. A pass that surveys, spends a model call and changes
		// nothing looks identical from the outside to a pass that never ran,
		// which is how this one sat disabled behind an early return for weeks
		// while duplicates piled up.
		mg.log.Info("memory-merge: nothing consolidated",
			zap.String("subject", bestKey), zap.Int("considered", len(batch)),
			zap.Int("clusters_proposed", len(res.Merges)))
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

// listEntries reads the memory this instance actually uses.
func (mg *Merger) listEntries(ctx context.Context) ([]store.StoredMemoryEntry, error) {
	if mg.mem != nil && mg.mem.HasRemote() {
		remote, err := mg.mem.Survey(ctx, 2000)
		if err != nil {
			return nil, fmt.Errorf("survey memory: %w", err)
		}
		out := make([]store.StoredMemoryEntry, 0, len(remote))
		for _, e := range remote {
			out = append(out, store.StoredMemoryEntry{
				ID: e.ID, Content: e.Content, Category: e.Category,
				Importance: e.Importance, Pinned: e.Pinned,
			})
		}
		return out, nil
	}
	entries, err := mg.store.ListMemoryEntries(mg.cfg.Namespace, 2000)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	return entries, nil
}

// subjectKey is the stable subject a memory is about, used to cluster
// near-duplicates that the store filed under different folders.
//
// The path's filename carries it: "facts/decisions/shravan.md" and
// "facts/people/shravan-kumar-scripting-collab-dropped-aug-11-d7f18387.md" are
// both about "shravan". The hash suffix and a leading date are stripped, and
// the first two meaningful slug tokens are kept — enough to bind a person or
// project together without merging two different ones.
func subjectKey(e store.StoredMemoryEntry) string {
	base := e.ID
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.IndexByte(base, '#'); i >= 0 {
		base = base[:i] // drop a section slug
	}
	base = strings.TrimSuffix(base, ".md")
	// Drop a trailing 8-hex content hash.
	if i := strings.LastIndexByte(base, '-'); i >= 0 && isHex(base[i+1:]) {
		base = base[:i]
	}
	toks := strings.Split(base, "-")
	// Drop a leading ISO date (2026-08-13-...).
	for len(toks) > 0 && isNumericTok(toks[0]) {
		toks = toks[1:]
	}
	// The FIRST token only. Two tokens split the very clusters this exists to
	// gather: "shravan-kumar", "shravan-nalacharla" and "shravan-scripting" are
	// one person filed three ways. A broad key is safe because the model still
	// reads each entry and merges only what it judges duplicate or superseded —
	// clustering decides what gets COMPARED, not what gets combined.
	key := ""
	if len(toks) > 0 {
		key = toks[0]
	}
	if key == "" {
		// No usable filename: fall back to the leading words of the content, so
		// the entry still clusters with its own wording-twins rather than
		// becoming a singleton.
		words := strings.Fields(strings.ToLower(oneLine(e.Content, 60)))
		if len(words) > 2 {
			words = words[:2]
		}
		key = strings.Join(words, "-")
	}
	return key
}

func isHex(s string) bool {
	if len(s) < 6 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func isNumericTok(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// modalCategory returns the category most of a cluster's entries carry, so a
// merged fact is filed where the subject already mostly lives.
func modalCategory(batch []store.StoredMemoryEntry) string {
	counts := map[string]int{}
	best, bestN := "context", 0
	for _, e := range batch {
		c := e.Category
		if c == "" || c == "facts" {
			continue // the coarse tier is no signal
		}
		counts[c]++
		if counts[c] > bestN {
			best, bestN = c, counts[c]
		}
	}
	return best
}

// subjectTag makes the subject key usable as the merged fact's first tag, so it
// re-clusters with its own kind on the next pass instead of scattering again.
func subjectTag(key string) string {
	if key == "" {
		return "merged"
	}
	return key
}
