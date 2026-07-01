package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
)

// MemoryForgetTool lets the agent curate its own long-term memory by removing a
// fact that is wrong, outdated, or superseded — making memory self-correcting
// rather than append-only.
type MemoryForgetTool struct {
	Store     *store.Store
	MemoryMgr *memory.Manager
	AgentID   string
}

func (t *MemoryForgetTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "memory.forget",
		Description: "Remove a long-term memory that is wrong, outdated, or superseded, so your memory stays accurate. Pass 'id' to delete a specific entry, or 'query' to find and remove the best-matching memory (other close matches are listed, not deleted). When correcting a fact, forget the stale one and memory.ingest the corrected version.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "Exact memory entry id to delete (most precise)."},
				"query": {"type": "string", "description": "Text describing the memory to remove; the single best match is deleted."}
			}
		}`),
	}
}

func (t *MemoryForgetTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	id, _ := input["id"].(string)
	query, _ := input["query"].(string)
	id = strings.TrimSpace(id)
	query = strings.TrimSpace(query)

	if id != "" {
		if err := t.Store.DeleteMemoryEntry(id); err != nil {
			return tools.ErrorResult(fmt.Errorf("delete memory %s: %w", id, err)), nil
		}
		return tools.SuccessResult(fmt.Sprintf("Forgot memory %s.", id)), nil
	}

	if query == "" {
		return tools.ErrorResult(fmt.Errorf("provide 'id' or 'query'")), nil
	}

	results, err := t.MemoryMgr.SearchSemantic(query, 5)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("search: %w", err)), nil
	}

	// Only real entries carry a deletable id; skip pageindex nodes (role "memory").
	var top *memory.SearchResult
	var others []string
	for i := range results {
		r := results[i]
		if r.Entry.ID == "" || r.Entry.Role == "memory" {
			continue
		}
		if top == nil {
			r := r
			top = &r
			continue
		}
		others = append(others, fmt.Sprintf("%s — %s", r.Entry.ID, truncateStr(stripTagPrefix(r.Entry.Content), 70)))
	}
	if top == nil {
		return tools.SuccessResult("No matching memory found to forget."), nil
	}
	if err := t.Store.DeleteMemoryEntry(top.Entry.ID); err != nil {
		return tools.ErrorResult(fmt.Errorf("delete memory %s: %w", top.Entry.ID, err)), nil
	}
	msg := fmt.Sprintf("Forgot: %q (id %s).", truncateStr(stripTagPrefix(top.Entry.Content), 100), top.Entry.ID)
	if len(others) > 0 {
		msg += " Other close matches were NOT deleted (pass their id to forget too): " + strings.Join(others, "; ")
	}
	return tools.SuccessResult(msg), nil
}

// stripTagPrefix removes leading "[category][importance]" tags for display.
func stripTagPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "[") {
		i := strings.Index(s, "]")
		if i < 0 {
			break
		}
		s = strings.TrimSpace(s[i+1:])
	}
	return s
}
