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

// MemoryIngestTool lets the agent save important information to long-term
// memory with automatic deduplication against existing entries.
type MemoryIngestTool struct {
	Store     *store.Store
	MemoryMgr *memory.Manager // set per-agent at registration time
	AgentID   string
}

func (t *MemoryIngestTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "memory.ingest",
		Description: "Save important information to long-term memory. Use this to remember facts about the user, decisions, project context, preferences, and anything that should persist across conversations. The system automatically checks for duplicates to avoid storing redundant information.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"content": {"type": "string", "description": "The information to remember. Be specific and structured."},
				"category": {"type": "string", "description": "Category: 'user_info', 'project', 'decision', 'preference', 'context', 'task', 'relationship'"},
				"tags": {"type": "string", "description": "Comma-separated tags for organization (e.g., 'kartik,preferences,coding')"},
				"importance": {"type": "string", "description": "Priority: 'critical', 'high', 'medium', 'low'"}
			},
			"required": ["content", "category"]
		}`),
	}
}

func (t *MemoryIngestTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	content, _ := input["content"].(string)
	category, _ := input["category"].(string)
	tagsRaw, _ := input["tags"].(string)
	importance, _ := input["importance"].(string)

	if content == "" {
		return tools.ErrorResult(fmt.Errorf("content is required")), nil
	}
	if category == "" {
		return tools.ErrorResult(fmt.Errorf("category is required")), nil
	}
	if importance == "" {
		importance = "medium"
	}

	// Parse tags.
	var tags []string
	if tagsRaw != "" {
		for _, tag := range strings.Split(tagsRaw, ",") {
			tag = strings.TrimSpace(tag)
			if tag != "" {
				tags = append(tags, tag)
			}
		}
	}
	tags = append(tags, category)

	namespace := t.MemoryMgr.Namespace()

	// --- Deduplication check ---
	searchKey := content
	if len(searchKey) > 50 {
		searchKey = searchKey[:50]
	}

	// Search existing memory entries.
	existingEntries, _ := t.Store.SearchMemoryEntries(namespace, searchKey, 20)
	for _, e := range existingEntries {
		sim := wordOverlap(content, e.Content)
		if sim > 0.7 {
			excerpt := e.Content
			if len(excerpt) > 120 {
				excerpt = excerpt[:120] + "..."
			}
			return tools.SuccessResult(fmt.Sprintf("Similar memory already exists: %q. Skipping.", excerpt)), nil
		}
	}

	// Search pageindex nodes.
	existingNodes, _ := t.Store.SearchPageIndexNodes(namespace, searchKey, 20)
	for _, n := range existingNodes {
		sim := wordOverlap(content, n.SearchText)
		if sim > 0.7 {
			excerpt := n.SearchText
			if len(excerpt) > 120 {
				excerpt = excerpt[:120] + "..."
			}
			return tools.SuccessResult(fmt.Sprintf("Similar memory already exists: %q. Skipping.", excerpt)), nil
		}
	}

	// Check for partial matches — warn but still save.
	var partialWarning string
	for _, e := range existingEntries {
		sim := wordOverlap(content, e.Content)
		if sim >= 0.4 && sim <= 0.7 {
			excerpt := e.Content
			if len(excerpt) > 80 {
				excerpt = excerpt[:80] + "..."
			}
			partialWarning = fmt.Sprintf(" (Note: partial match found with existing memory: %q, similarity=%.0f%%)", excerpt, sim*100)
			break
		}
	}

	// Write to memory.
	tagsJSON, _ := json.Marshal(tags)
	formattedContent := fmt.Sprintf("[%s][%s] %s", category, importance, content)

	entry := memory.MemoryEntry{
		AgentID:   t.AgentID,
		Namespace: namespace,
		Role:      "system",
		Content:   formattedContent,
		Tags:      tags,
	}

	if err := t.MemoryMgr.Write(entry); err != nil {
		return tools.ErrorResult(fmt.Errorf("failed to write memory: %w", err)), nil
	}

	_ = tagsJSON // tags already passed via entry.Tags

	return tools.SuccessResult(fmt.Sprintf("Memory saved [%s/%s]: %s%s",
		category, importance, truncateStr(content, 80), partialWarning)), nil
}

// wordOverlap computes normalized Jaccard similarity on whitespace-split word sets.
func wordOverlap(a, b string) float64 {
	setA := wordSet(strings.ToLower(a))
	setB := wordSet(strings.ToLower(b))

	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}

	intersection := 0
	for w := range setA {
		if setB[w] {
			intersection++
		}
	}

	union := len(setA)
	for w := range setB {
		if !setA[w] {
			union++
		}
	}

	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func wordSet(s string) map[string]bool {
	set := make(map[string]bool)
	for _, w := range strings.Fields(s) {
		w = strings.Trim(w, ".,;:!?\"'()[]{}")
		if w != "" {
			set[w] = true
		}
	}
	return set
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
