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
	if !rev.Answerable() {
		// Already answered elsewhere — reply-anywhere means this is fine. A
		// question that expired unanswered is still answerable: the operator's
		// answer is taken whenever it comes.
		return tools.SuccessResult(map[string]any{"status": "already_resolved"}), nil
	}

	status := "resolved"
	if resolution == "dropped" || resolution == "forgotten" {
		status = "dismissed"
	}
	if err := t.Store.ResolveReview(id, status, answer, resolution); err != nil {
		return tools.ErrorResult(fmt.Errorf("resolve review: %w", err)), nil
	}

	// Apply the consequence to the underlying MEMORY (reminders just close).
	//
	// Through the manager, not the table. The operator has just been asked "is
	// this still true?" and answered no — deleting a row in a store nothing
	// reads would report "memory forgotten" and leave the fact in place to be
	// recalled tomorrow, which is worse than never having asked.
	applied := "review closed"
	if rev.TargetKind == "memory" && rev.TargetID != "" && t.MemoryMgr != nil {
		switch resolution {
		case "forgotten", "done", "dropped":
			// ForgetFact, not Forget: the handle names a SUBJECT, and the
			// operator answered a question about one item under it.
			removed, derr := t.MemoryMgr.ForgetFact(ctx, rev.TargetID)
			switch {
			case derr != nil:
				applied = "review closed; the memory could not be removed: " + derr.Error()
			case removed:
				applied = "review closed; memory forgotten"
			default:
				applied = "review closed; the memory was KEPT — it holds several " +
					"facts about this subject and only one of them was asked about. " +
					"Tell the operator, and use memory.forget on the specific fact if they want it gone."
			}
		case "updated":
			if strings.TrimSpace(newContent) != "" {
				// Correct, not Update: the handle may name a subject whose
				// document holds many facts, and rewriting all of it with the
				// one corrected sentence erased the rest. Correct replaces a
				// single fact, and adds the correction beside a subject's
				// others — keeping the tags, cues and links that make it
				// findable either way.
				replaced, uerr := t.MemoryMgr.Correct(ctx, rev.TargetID, strings.TrimSpace(newContent))
				switch {
				case uerr != nil:
					applied = "review closed; the memory could not be updated: " + uerr.Error()
				case replaced:
					applied = "review closed; memory updated"
				default:
					applied = "review closed; the correction was added beside the subject's other facts"
				}
			}
		}
	}

	return tools.SuccessResult(map[string]any{
		"status":     "resolved",
		"resolution": resolution,
		"applied":    applied,
	}), nil
}
