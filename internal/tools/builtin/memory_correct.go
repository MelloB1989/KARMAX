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

// MemoryCorrectTool replaces a fact in long-term memory with its correction.
//
// A correction used to be two calls the agent had to remember to make in the
// right order — forget the old fact, ingest the new one — and the ingest half
// was often dropped as a near-duplicate of the fact it corrected. One call does
// both, through the manager, which knows whether the handle names one fact or a
// whole subject.
type MemoryCorrectTool struct {
	Store     *store.Store
	MemoryMgr *memory.Manager
	AgentID   string
}

func (t *MemoryCorrectTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "memory.correct",
		Description: "Replace a long-term memory that is wrong or out of date with its correction, in one step. Pass the memory's 'id' as memory.retrieve shows it (a section id, file.md#..., names one fact) and the full corrected fact as 'content', standalone and dated. Use it whenever the operator says a remembered fact is wrong or has changed.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "The memory to correct, exactly as memory.retrieve shows it."},
				"content": {"type": "string", "description": "The corrected fact, standalone: who, what, and when."}
			},
			"required": ["id", "content"]
		}`),
	}
}

func (t *MemoryCorrectTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	id, _ := input["id"].(string)
	content, _ := input["content"].(string)
	id, content = strings.TrimSpace(id), strings.TrimSpace(content)
	if id == "" || content == "" {
		return tools.ErrorResult(fmt.Errorf("provide the memory 'id' and the corrected 'content'")), nil
	}
	replaced, err := t.MemoryMgr.Correct(ctx, id, content)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("correct memory %s: %w", id, err)), nil
	}
	if replaced {
		return tools.SuccessResult(fmt.Sprintf("Corrected %s: %s", id, truncateStr(content, 100))), nil
	}
	return tools.SuccessResult(fmt.Sprintf("%s holds several facts, so the correction was added to it as the newest one rather than replacing them all: %s. If one old fact there is now wrong, forget it by its section id.",
		id, truncateStr(content, 100))), nil
}
