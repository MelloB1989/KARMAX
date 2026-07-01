package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
		Description: "Save durable information to long-term memory: facts about the operator, decisions, project state, preferences, people, commitments — anything worth recalling across conversations. Duplicates are detected automatically. Set 'importance' so it ranks correctly, 'pinned' for facts that must never be forgotten, and 'ttl_days' for facts that expire (e.g. a temporary plan).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"content": {"type": "string", "description": "The information to remember. Be specific, atomic, and self-contained (it will be read without conversation context)."},
				"category": {"type": "string", "description": "Category: 'user_info', 'project', 'decision', 'preference', 'context', 'task', 'relationship'"},
				"tags": {"type": "string", "description": "Comma-separated tags for organization (e.g., 'preferences,coding')"},
				"importance": {"type": "string", "description": "Priority: 'critical', 'high', 'medium', 'low' (default 'medium'). Drives recall ranking and what survives forgetting."},
				"pinned": {"type": "boolean", "description": "If true, this memory is never auto-forgotten and is always front-of-mind (use for core, enduring facts)."},
				"ttl_days": {"type": "integer", "description": "Optional: auto-expire this memory after N days (use for time-bound facts like a temporary plan or deadline)."}
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
	pinned, _ := input["pinned"].(bool)

	if content == "" {
		return tools.ErrorResult(fmt.Errorf("content is required")), nil
	}
	if category == "" {
		return tools.ErrorResult(fmt.Errorf("category is required")), nil
	}
	if importance == "" {
		importance = "medium"
	}

	// Optional TTL → expiry timestamp.
	var expiresAt *time.Time
	switch v := input["ttl_days"].(type) {
	case float64:
		if v > 0 {
			t := time.Now().AddDate(0, 0, int(v))
			expiresAt = &t
		}
	case int:
		if v > 0 {
			t := time.Now().AddDate(0, 0, v)
			expiresAt = &t
		}
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

	// Write to memory. The [category][importance] prefix is kept for the human
	// -readable tree/markdown; the structured fields drive ranking & forgetting.
	formattedContent := fmt.Sprintf("[%s][%s] %s", category, importance, content)

	entry := memory.MemoryEntry{
		AgentID:    t.AgentID,
		Namespace:  namespace,
		Role:       "system",
		Content:    formattedContent,
		Tags:       tags,
		Category:   category,
		Importance: importanceToInt(importance),
		Pinned:     pinned,
		ExpiresAt:  expiresAt,
	}

	if err := t.MemoryMgr.Write(entry); err != nil {
		return tools.ErrorResult(fmt.Errorf("failed to write memory: %w", err)), nil
	}

	extra := partialWarning
	if pinned {
		extra += " (pinned)"
	}
	if expiresAt != nil {
		extra += fmt.Sprintf(" (expires %s)", expiresAt.Format("2006-01-02"))
	}
	return tools.SuccessResult(fmt.Sprintf("Memory saved [%s/%s]: %s%s",
		category, importance, truncateStr(content, 80), extra)), nil
}

// importanceToInt maps the importance label to its stored priority (1=low..4=critical).
func importanceToInt(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return 4
	case "high":
		return 3
	case "low":
		return 1
	default:
		return 2 // medium
	}
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
