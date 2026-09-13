package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
)

// TaskStartTool registers a long-running background job as a tasks row, so it
// has somewhere to report progress and a status a later turn can pause or
// cancel. Configured timeouts (chat 4m, agent 12m, task 20m) rule out doing
// the actual work inside a turn — a paced send over hours is the motivating
// case — so this is what a turn calls before handing the work to a detached
// process, giving that process the task id it needs to check in.
type TaskStartTool struct {
	Store   *store.Store
	AgentID string
}

func (t *TaskStartTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "task.start",
		Description: "Register a long-running background job — anything that will outlive a turn, like a paced " +
			"send over many minutes or hours — as a tracked task. Returns a task_id: pass it to the detached " +
			"process you start next so it can report progress and check whether it has been paused or " +
			"cancelled. Call this BEFORE starting the actual work, not after.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"goal": {"type": "string", "description": "What this run is doing, in the operator's terms."},
				"title": {"type": "string", "description": "Short label (optional; derived from goal if omitted)."},
				"target": {"type": "string", "description": "What or who this concerns, e.g. a post URL or an account (optional)."},
				"workdir": {"type": "string", "description": "The detached process's working directory (optional)."},
				"total": {"type": "integer", "description": "Known size of the run, e.g. a recipient count, if already known (optional)."}
			},
			"required": ["goal"]
		}`),
	}
}

func (t *TaskStartTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	goal, _ := input["goal"].(string)
	if strings.TrimSpace(goal) == "" {
		return tools.ErrorResult(fmt.Errorf("goal is required")), nil
	}
	title, _ := input["title"].(string)
	target, _ := input["target"].(string)
	workdir, _ := input["workdir"].(string)
	total := clampInt(input["total"], 0, 0, 1_000_000)

	task, err := t.Store.CreateTask(store.Task{
		AgentID:  t.AgentID,
		Title:    title,
		Goal:     goal,
		Status:   store.TaskRunning,
		Target:   target,
		Workdir:  workdir,
		Progress: store.EncodeProgress(store.TaskProgress{Total: total}),
	})
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("register task: %w", err)), nil
	}

	return tools.SuccessResult(map[string]any{
		"status":  "started",
		"task_id": task.ID,
		"total":   total,
		"note": "Pass this task_id to the detached process. It should write progress and re-read status " +
			"before each unit of work: paused means idle without exiting, cancelled means stop cleanly.",
	}), nil
}
