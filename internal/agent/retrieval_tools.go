package agent

import (
	"time"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
)

// These tools are given to the memory-retrieval sub-agent (see memorymodel.go).
// They let it query the memory database, the page-index tree, the cold chat
// summaries, and the operator profile, so it can assemble accurate context for
// the main orchestration agent across multiple autonomous queries.

func intOr(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return def
}

func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// memSearchTool — semantic keyword search over long-term memory entries.
type memSearchTool struct{ mem *memory.Manager }

func (t *memSearchTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "mem_search",
		Description: "Semantic search over the operator's long-term memory (facts, decisions, projects, people, tasks, preferences). " +
			"Run several focused queries with different keywords to be thorough.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer","description":"default 12"}},"required":["query"]}`),
	}
}

func (t *memSearchTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	q, _ := in["query"].(string)
	res, err := t.mem.SearchSemantic(q, intOr(in["limit"], 12))
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if len(res) == 0 {
		return tools.SuccessResult("no matching memory entries"), nil
	}
	var sb strings.Builder
	for _, r := range res {
		sb.WriteString(fmt.Sprintf("- [stored %s] (%.0f%%) %s\n", humanAge(r.Entry.CreatedAt), r.Score*100, oneLine(r.Entry.Content, 260)))
	}
	return tools.SuccessResult(sb.String()), nil
}

// humanAge renders how long ago t was, so the agent can reason about staleness
// (a "deadline Friday" stored 3 weeks ago is almost certainly resolved/missed).
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < 36*time.Hour:
		if d < 18*time.Hour {
			return "today"
		}
		return "yesterday"
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%d weeks ago", int(d.Hours()/(24*7)))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%d months ago", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%d+ months ago", int(d.Hours()/(24*30)))
	}
}

// memRecentTool — the freshest memory entries (latest hot context).
type memRecentTool struct{ mem *memory.Manager }

func (t *memRecentTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "mem_recent",
		Description: "Return the most recently stored memory entries — the freshest 'hot' context about what's currently going on.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","description":"default 15"}}}`),
	}
}

func (t *memRecentTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	entries, err := t.mem.Recent(intOr(in["limit"], 15))
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if len(entries) == 0 {
		return tools.SuccessResult("no memory entries yet"), nil
	}
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("- [stored %s] %s\n", humanAge(e.CreatedAt), oneLine(e.Content, 240)))
	}
	return tools.SuccessResult(sb.String()), nil
}

// pageIndexTool — search the structured page-index tree of memory.
type pageIndexTool struct {
	store     *store.Store
	namespace string
}

func (t *pageIndexTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "tree_search",
		Description: "Search the page-index tree (the structured/categorized index of memory) for matching nodes and their summaries.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer","description":"default 10"}},"required":["query"]}`),
	}
}

func (t *pageIndexTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	q, _ := in["query"].(string)
	nodes, err := t.store.SearchPageIndexNodes(t.namespace, q, intOr(in["limit"], 10))
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if len(nodes) == 0 {
		return tools.SuccessResult("no matching tree nodes"), nil
	}
	var sb strings.Builder
	for _, n := range nodes {
		s := n.Summary
		if s == "" {
			s = n.SearchText
		}
		sb.WriteString(fmt.Sprintf("- %s: %s\n", n.Title, oneLine(s, 220)))
	}
	return tools.SuccessResult(sb.String()), nil
}

// chatSummaryTool — search the cold per-chat background summaries.
type chatSummaryTool struct{ store *store.Store }

func (t *chatSummaryTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "chat_summaries",
		Description: "Search the background ('cold') summaries of older WhatsApp conversations — useful for context about people the operator spoke to weeks or months ago.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer","description":"default 8"}},"required":["query"]}`),
	}
}

func (t *chatSummaryTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	q, _ := in["query"].(string)
	sums, err := t.store.SearchChatSummaries(q, intOr(in["limit"], 8))
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if len(sums) == 0 {
		return tools.SuccessResult("no matching chat summaries"), nil
	}
	var sb strings.Builder
	for _, c := range sums {
		sb.WriteString(fmt.Sprintf("### %s\n%s\n\n", c.ChatName, oneLine(c.Summary, 600)))
	}
	return tools.SuccessResult(sb.String()), nil
}

// profileReadTool — read the curated ABOUT_ME operator profile.
type profileReadTool struct{ mem *memory.Manager }

func (t *profileReadTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "profile_read",
		Description: "Read the curated ABOUT_ME profile (the authoritative document about the operator: identity, projects, preferences, relationships, goals).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (t *profileReadTool) Execute(_ context.Context, _ map[string]any) (tools.ToolResult, error) {
	p, err := t.mem.ReadProfile()
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if strings.TrimSpace(p) == "" {
		return tools.SuccessResult("profile is empty"), nil
	}
	return tools.SuccessResult(p), nil
}
