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

// ReviewResolveTool lets the agent close a staleness check-in ("is this still
// relevant?") using the operator's reply — from WhatsApp, the app, anywhere.
// The open reviews are injected into the agent's context (see agent.go); when
// the operator answers, the agent calls this to record the answer and apply the
// consequence to memory (keep / update / forget).
type ReviewResolveTool struct {
	Store     *store.Store
	MemoryMgr *memory.Manager
	AgentID   string
	Namespace string
}

func (t *ReviewResolveTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "review.resolve",
		Description: "Close a staleness check-in (a '🕰️ Still relevant?' question shown in '## Open review questions') using the operator's answer. " +
			"Call this when the operator replies to such a question in ANY channel. Pick the matching review by id. " +
			"Set resolution to what their answer means: 'kept' (still valid, leave the memory), 'updated' (revise it — provide new_content), " +
			"'forgotten'/'done'/'dropped' (no longer relevant — the underlying memory is removed).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "The review id from '## Open review questions'."},
				"answer": {"type": "string", "description": "The operator's answer, verbatim or summarized."},
				"resolution": {"type": "string", "enum": ["kept","updated","forgotten","done","dropped"], "description": "What the answer means for the memory."},
				"new_content": {"type": "string", "description": "For resolution=updated: the corrected memory text."}
			},
			"required": ["id", "resolution"]
		}`),
	}
}

func (t *ReviewResolveTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	id, _ := input["id"].(string)
	if strings.TrimSpace(id) == "" {
		return tools.ErrorResult(fmt.Errorf("id is required")), nil
	}
	answer, _ := input["answer"].(string)
	resolution, _ := input["resolution"].(string)
	newContent, _ := input["new_content"].(string)

	rev, err := t.Store.GetReview(id)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("load review: %w", err)), nil
	}
	if rev == nil {
		return tools.ErrorResult(fmt.Errorf("no review %s", id)), nil
	}
	if rev.Status != "open" {
		// Already answered elsewhere — reply-anywhere means this is fine.
		return tools.SuccessResult(map[string]any{"status": "already_resolved"}), nil
	}

	status := "resolved"
	if resolution == "dropped" || resolution == "forgotten" {
		status = "dismissed"
	}
	if err := t.Store.ResolveReview(id, status, answer, resolution); err != nil {
		return tools.ErrorResult(fmt.Errorf("resolve review: %w", err)), nil
	}

	// Apply the consequence to the underlying MEMORY entry (reminders just close).
	applied := "review closed"
	if rev.TargetKind == "memory" && rev.TargetID != "" && t.MemoryMgr != nil {
		switch resolution {
		case "forgotten", "done", "dropped":
			if derr := t.Store.DeleteMemoryEntry(rev.TargetID); derr == nil {
				applied = "review closed; memory forgotten"
			}
		case "updated":
			if strings.TrimSpace(newContent) != "" {
				_ = t.Store.DeleteMemoryEntry(rev.TargetID)
				_ = t.MemoryMgr.Write(memory.MemoryEntry{Role: "assistant", Content: strings.TrimSpace(newContent), Tags: []string{"reviewed"}})
				applied = "review closed; memory updated"
			}
		}
	}

	return tools.SuccessResult(map[string]any{
		"status":     "resolved",
		"resolution": resolution,
		"applied":    applied,
	}), nil
}
