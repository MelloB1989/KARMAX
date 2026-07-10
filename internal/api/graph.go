package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"go.uber.org/zap"
)

// The memory graph: an LLM maps relationships between memory entries (same
// person, part of a project, cause/effect, dependencies, …). Stored in
// memory_links and rendered as a graph in the app's 3D memory view.

// graphNodeLimit caps how many (most-recent) memory entries the 3D graph
// renders as NODES. Kept high so the graph tracks the real, growing memory —
// the old hardcoded 55 (then 160) made the count look permanently frozen.
const graphNodeLimit = 1200

// graphLinkLimit caps how many entries are fed to the LLM link generator. The
// relationship prompt has to stay a sane size, so links are computed over the
// most-recent graphLinkLimit entries even when more nodes are shown.
const graphLinkLimit = 160

const graphLinkPrompt = `You map relationships between an operator's memory entries. You are given entries (id + content). Output the meaningful relationships between them.

Only link entries that are genuinely related — the same person, the same project, an event and its participants, a decision and what it affects, cause/effect, or dependencies. Use a SHORT relation label (1-3 words), e.g. "same person", "part of", "works on", "depends on", "led to", "scheduled for".

Respond with ONLY a JSON array (max 90 links), no prose:
[{"from":"<id>","to":"<id>","relation":"<label>"}]
If nothing is meaningfully related, respond with exactly: []`

func extractJSONArray(s string) string {
	i := strings.IndexByte(s, '[')
	j := strings.LastIndexByte(s, ']')
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return ""
}

// graphTitle is a short label for a memory node (strips [cat][imp] tags).
func graphTitle(content string) string {
	s := strings.TrimSpace(content)
	for strings.HasPrefix(s, "[") {
		k := strings.IndexByte(s, ']')
		if k < 0 {
			break
		}
		s = strings.TrimSpace(s[k+1:])
	}
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 70 {
		s = s[:70] + "…"
	}
	return s
}

func categoryFromContent(content string) string {
	c := strings.TrimSpace(content)
	if strings.HasPrefix(c, "[") {
		if i := strings.IndexByte(c, ']'); i > 1 {
			return strings.TrimSpace(c[1:i])
		}
	}
	return "context"
}

// generateGraphLinks runs the model over the entries and returns validated links.
func (s *Server) generateGraphLinks(ctx context.Context, entries []store.StoredMemoryEntry) []store.MemoryLink {
	// Present entries with SHORT ids (n0, n1, …) — the model can't reliably echo
	// 36-char UUIDs — and translate the returned links back to real ids.
	var sb strings.Builder
	idMap := map[string]string{}
	i := 0
	for _, e := range entries {
		if isJunkMemory(e.Content) {
			continue
		}
		short := fmt.Sprintf("n%d", i)
		i++
		idMap[short] = e.ID
		sb.WriteString(fmt.Sprintf("- %s :: %s\n", short, cleanupTruncate(e.Content, 130)))
	}
	if len(idMap) < 2 {
		return nil
	}
	resp := s.graphLinkResponse(ctx, sb.String())
	if strings.TrimSpace(resp) == "" {
		return nil
	}

	var raw []store.MemoryLink
	_ = json.Unmarshal([]byte(extractJSONArray(resp)), &raw)
	var out []store.MemoryLink
	seen := map[string]bool{}
	for _, l := range raw {
		from, okF := idMap[strings.TrimSpace(l.FromID)]
		to, okT := idMap[strings.TrimSpace(l.ToID)]
		if !okF || !okT || from == to {
			continue
		}
		key := from + "|" + to + "|" + l.Relation
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, store.MemoryLink{FromID: from, ToID: to, Relation: l.Relation})
	}
	return out
}

// graphLinkResponse gets the raw model output for the relationship graph. The
// graph is a background, latency-insensitive maintenance task, so reliability
// beats speed: it goes through the claude_code harness first (which has credits
// and completes even when the kiro gateway brain is timing out — the same
// escape hatch tech-news uses), and only falls back to the cleanup model if the
// harness is unavailable. This is why the graph used to freeze: the cleanup
// model on the flaky gateway kept failing, and nothing else ever retried.
func (s *Server) graphLinkResponse(ctx context.Context, entriesBlock string) string {
	body := "Entries:\n" + entriesBlock

	// Primary: claude_code harness.
	agentID, ns := "", s.defaultNamespace()
	if len(s.cfg.Agents) > 0 {
		agentID = s.cfg.Agents[0].ID
		if n := s.cfg.Agents[0].Memory.Namespace; n != "" {
			ns = n
		}
	}
	if agentID != "" {
		cc := &builtin.ClaudeCodeTool{Store: s.store, AgentID: agentID, Namespace: ns}
		prompt := graphLinkPrompt + "\n\n" + body +
			"\n\nOutput ONLY the JSON array described above — nothing else."
		hctx, cancel := context.WithTimeout(ctx, 120*time.Second)
		res, err := cc.Execute(hctx, map[string]any{"prompt": prompt, "ephemeral": true})
		cancel()
		if err == nil && !res.IsError {
			if m, ok := res.Output.(map[string]any); ok {
				if out, _ := m["output"].(string); strings.TrimSpace(extractJSONArray(out)) != "" {
					return out
				}
			}
		}
		s.log.Warn("graph links: claude_code harness produced no usable output, falling back to cleanup model")
	}

	// Fallback: the cleanup/summary model (may time out on the kiro gateway).
	resp, _, _, err := s.cleanupSession(graphLinkPrompt).Chat(ctx, body)
	if err != nil {
		return ""
	}
	return resp
}

func (s *Server) graphNodes(entries []store.StoredMemoryEntry) []map[string]any {
	nodes := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if isJunkMemory(e.Content) {
			continue
		}
		nodes = append(nodes, map[string]any{
			"id":       e.ID,
			"title":    graphTitle(e.Content),
			"content":  e.Content,
			"category": categoryFromContent(e.Content),
		})
	}
	return nodes
}

func (s *Server) handleMemoryGraph(w http.ResponseWriter, r *http.Request) {
	ns := s.defaultNamespace()
	entries, err := s.store.ListMemoryEntries(ns, graphNodeLimit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	links, _ := s.store.ListMemoryLinks()
	// First time (no links yet): generate synchronously so the graph isn't empty.
	if len(links) == 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		if gen := s.generateGraphLinks(ctx, linkEntries(entries)); len(gen) > 0 {
			_ = s.store.ReplaceMemoryLinks(gen)
			links = gen
		}
	}
	if links == nil {
		links = []store.MemoryLink{}
	}
	total, _ := s.store.CountMemoryEntries(ns)
	nodes := s.graphNodes(entries)
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": nodes,
		"links": links,
		// The true memory size, so the app shows real growth even though the
		// graph renders at most graphNodeLimit nodes.
		"total":  total,
		"shown":  len(nodes),
		"capped": total > len(nodes),
	})
}

// linkEntries returns the slice of entries used for link generation: the most
// recent graphLinkLimit (ListMemoryEntries is ordered newest-first).
func linkEntries(entries []store.StoredMemoryEntry) []store.StoredMemoryEntry {
	if len(entries) > graphLinkLimit {
		return entries[:graphLinkLimit]
	}
	return entries
}

func (s *Server) handleRebuildGraph(w http.ResponseWriter, r *http.Request) {
	ns := s.defaultNamespace()
	entries, err := s.store.ListMemoryEntries(ns, graphNodeLimit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	defer cancel()
	total, _ := s.store.CountMemoryEntries(ns)
	links := s.generateGraphLinks(ctx, linkEntries(entries))
	if links == nil {
		// Generation failed (model/harness hiccup) — DON'T wipe the existing
		// graph. Return what's currently stored so the app keeps its links.
		existing, _ := s.store.ListMemoryLinks()
		if existing == nil {
			existing = []store.MemoryLink{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": s.graphNodes(entries), "links": existing, "total": total, "note": "link generation unavailable; kept existing links"})
		return
	}
	if err := s.store.ReplaceMemoryLinks(links); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": s.graphNodes(entries), "links": links, "total": total})
}

// startGraphMaintainer keeps memory_links fresh. The links used to be generated
// exactly once (the first time the app opened the graph with zero links) and
// never again — so as memory grew, every new entry showed up as an unlinked
// dot and the graph looked frozen. This rebuilds the relationship set on a
// timer, but only when the memory set has actually changed (or links are
// missing / very old), so it doesn't burn model calls on an idle graph.
func (s *Server) startGraphMaintainer(ctx context.Context) {
	const (
		firstDelay = 90 * time.Second
		interval   = 2 * time.Hour
		maxLinkAge = 12 * time.Hour
	)
	lastCount := -1
	rebuild := func(reason string) {
		ns := s.defaultNamespace()
		entries, err := s.store.ListMemoryEntries(ns, graphNodeLimit)
		if err != nil || len(entries) < 2 {
			return
		}
		rctx, cancel := context.WithTimeout(ctx, 150*time.Second)
		defer cancel()
		links := s.generateGraphLinks(rctx, linkEntries(entries))
		if links == nil {
			// model hiccup — don't wipe the existing graph
			return
		}
		if err := s.store.ReplaceMemoryLinks(links); err != nil {
			s.log.Warn("graph maintainer: replace links failed", zap.Error(err))
			return
		}
		lastCount = len(entries)
		s.log.Info("graph maintainer rebuilt links", zap.String("reason", reason), zap.Int("nodes", len(entries)), zap.Int("links", len(links)))
	}

	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(firstDelay):
		}
		// On startup, refresh if links are stale/missing relative to memory.
		if links, _ := s.store.ListMemoryLinks(); len(links) == 0 {
			rebuild("startup: no links")
		} else if age := s.store.OldestMemoryLinkAge(); age < 0 || age > maxLinkAge {
			rebuild("startup: links stale")
		} else if n, _ := s.store.CountMemoryEntries(s.defaultNamespace()); n != lastCount {
			lastCount = n // seed baseline without a rebuild if links are already fresh
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := s.store.CountMemoryEntries(s.defaultNamespace())
				if err != nil {
					continue
				}
				age := s.store.OldestMemoryLinkAge()
				if n != lastCount || age < 0 || age > maxLinkAge {
					rebuild("scheduled: memory changed or links aged")
				}
			}
		}
	}()
}
