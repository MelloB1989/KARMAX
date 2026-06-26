package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
)

// The memory graph: an LLM maps relationships between memory entries (same
// person, part of a project, cause/effect, dependencies, …). Stored in
// memory_links and rendered as a graph in the app's 3D memory view.

const graphLinkPrompt = `You map relationships between an operator's memory entries. You are given entries (id + content). Output the meaningful relationships between them.

Only link entries that are genuinely related — the same person, the same project, an event and its participants, a decision and what it affects, cause/effect, or dependencies. Use a SHORT relation label (1-3 words), e.g. "same person", "part of", "works on", "depends on", "led to", "scheduled for".

Respond with ONLY a JSON array (max 40 links), no prose:
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
	resp, _, _, err := s.cleanupSession(graphLinkPrompt).Chat(ctx, "Entries:\n"+sb.String())
	if err != nil {
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
	entries, err := s.store.ListMemoryEntries(ns, 55)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	links, _ := s.store.ListMemoryLinks()
	// First time (no links yet): generate synchronously so the graph isn't empty.
	if len(links) == 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		if gen := s.generateGraphLinks(ctx, entries); len(gen) > 0 {
			_ = s.store.ReplaceMemoryLinks(gen)
			links = gen
		}
	}
	if links == nil {
		links = []store.MemoryLink{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": s.graphNodes(entries), "links": links})
}

func (s *Server) handleRebuildGraph(w http.ResponseWriter, r *http.Request) {
	ns := s.defaultNamespace()
	entries, err := s.store.ListMemoryEntries(ns, 55)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	links := s.generateGraphLinks(ctx, entries)
	if err := s.store.ReplaceMemoryLinks(links); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": s.graphNodes(entries), "links": links})
}
