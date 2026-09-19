package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

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

// TaskProgressTool is the detached script's write. It reports one round of
// progress and, in the same call, hands back the task's current status — the
// control channel — so a paced send makes one tool call per recipient
// instead of a write followed by a separate read to check whether it has
// been paused or cancelled.
type TaskProgressTool struct {
	Store *store.Store
}

func (t *TaskProgressTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "task.progress",
		Description: "Report progress on a task started with task.start, and learn in the same call " +
			"whether to keep going: the result's status IS the control signal. \"running\" means continue. " +
			"\"paused\" means go idle without exiting — do not stop the process, just wait and call this " +
			"again later (e.g. on the next unit of work) to see if it changed. \"cancelled\" means stop " +
			"cleanly now; what you have already written here is kept, so a later resume still knows who " +
			"was reached. Call this once per unit of work — e.g. once per recipient in a paced send — not " +
			"as a separate write and then a separate status check.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"task_id": {"type": "string", "description": "From task.start."},
				"sent": {"type": "integer", "description": "How many units of work have succeeded so far."},
				"attempted": {"type": "integer", "description": "How many have been tried so far (>= sent). Write this before the attempt and sent after it, so a crash between the two is visible on resume."},
				"total": {"type": "integer", "description": "Known size of the run (optional; keeps the value already on the task if omitted)."}
			},
			"required": ["task_id", "sent", "attempted"]
		}`),
	}
}

func (t *TaskProgressTool) Execute(_ context.Context, input map[string]any) (tools.ToolResult, error) {
	taskID := strings.TrimSpace(asString(input["task_id"]))
	if taskID == "" {
		return tools.ErrorResult(fmt.Errorf("task_id is required")), nil
	}

	existing, found, err := t.Store.GetTask(taskID)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("read task: %w", err)), nil
	}
	if !found {
		return tools.ErrorResult(fmt.Errorf("no task with id %s", taskID)), nil
	}
	prev, _ := store.DecodeProgress(existing.Progress)

	total := prev.Total
	if _, given := input["total"]; given {
		total = clampInt(input["total"], total, 0, 1_000_000)
	}
	p := store.TaskProgress{
		Sent:      clampInt(input["sent"], prev.Sent, 0, 1_000_000),
		Attempted: clampInt(input["attempted"], prev.Attempted, 0, 1_000_000),
		Total:     total,
		LastAt:    time.Now().UTC(),
	}
	if err := t.Store.SetTaskProgress(taskID, p); err != nil {
		return tools.ErrorResult(fmt.Errorf("record progress: %w", err)), nil
	}

	// Re-read after the write rather than trusting `existing`: a pause or a
	// cancel landing between the GetTask above and the SetTaskProgress just
	// now must still be seen. Returning a status that was already stale
	// when we started would be the same bug this tool exists to close.
	current, found, err := t.Store.GetTask(taskID)
	status := existing.Status
	if err == nil && found {
		status = current.Status
	}
	return tools.SuccessResult(map[string]any{
		"status": status, "sent": p.Sent, "attempted": p.Attempted, "total": p.Total,
	}), nil
}

// TaskStatusTool is the pause button — spec §3b: "pausing is one UPDATE" —
// and, with no status given, a plain read of a task's status and progress.
// With no task_id at all it lists the operator's open long-running tasks, so
// a conversation that wants to pause something can find it first.
type TaskStatusTool struct {
	Store *store.Store
}

func (t *TaskStatusTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "task.status",
		Description: "Read or set a long-running task's status. Omit status to just read it, alongside " +
			"its progress. Set status to \"paused\" to make the detached process idle without exiting, " +
			"\"running\" to resume it, or \"cancelled\" to stop it for good — its progress ledger is kept, " +
			"so a later resume does not re-send to anyone already reached. Omit task_id entirely to list " +
			"the running/paused long tasks instead of reading one.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"task_id": {"type": "string", "description": "From task.start. Omit to list open long-running tasks instead."},
				"status": {"type": "string", "description": "running | paused | cancelled. Omit to read without changing anything."}
			}
		}`),
	}
}

func (t *TaskStatusTool) Execute(_ context.Context, input map[string]any) (tools.ToolResult, error) {
	taskID := strings.TrimSpace(asString(input["task_id"]))
	newStatus := strings.ToLower(strings.TrimSpace(asString(input["status"])))

	if taskID == "" {
		if newStatus != "" {
			return tools.ErrorResult(fmt.Errorf("status can only be set together with a task_id")), nil
		}
		return t.list()
	}

	if newStatus != "" {
		switch newStatus {
		case store.TaskRunning, store.TaskPaused, store.TaskCancelled:
		default:
			return tools.ErrorResult(fmt.Errorf(
				"status must be one of running, paused, cancelled — got %q", newStatus)), nil
		}
		if err := t.Store.SetTaskStatus(taskID, newStatus); err != nil {
			return tools.ErrorResult(fmt.Errorf("set status: %w", err)), nil
		}
	}

	task, found, err := t.Store.GetTask(taskID)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("read task: %w", err)), nil
	}
	if !found {
		return tools.ErrorResult(fmt.Errorf("no task with id %s", taskID)), nil
	}
	p, _ := store.DecodeProgress(task.Progress)
	return tools.SuccessResult(map[string]any{
		"task_id": task.ID, "title": task.Title, "status": task.Status,
		"sent": p.Sent, "attempted": p.Attempted, "total": p.Total,
	}), nil
}

// list surfaces the running and paused tasks — the ones a "pause X" or
// "how's X going" conversation is looking for. Two single-status reads
// against the existing (status, next_action_at) index rather than a new
// query shape or a new index.
func (t *TaskStatusTool) list() (tools.ToolResult, error) {
	running, err := t.Store.ListTasks(store.TaskRunning, 20)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("list running tasks: %w", err)), nil
	}
	paused, err := t.Store.ListTasks(store.TaskPaused, 20)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("list paused tasks: %w", err)), nil
	}
	rows := append(running, paused...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].UpdatedAt.After(rows[j].UpdatedAt) })

	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		p, _ := store.DecodeProgress(r.Progress)
		out = append(out, map[string]any{
			"task_id": r.ID, "title": r.Title, "status": r.Status,
			"sent": p.Sent, "attempted": p.Attempted, "total": p.Total,
		})
	}
	return tools.SuccessResult(map[string]any{"tasks": out, "count": len(out)}), nil
}

// asString reads a tool input field that a shell caller can only ever have
// sent as a string — cmd/karmax's key=value parser only produces something
// else (float64, bool, nil) when the value happens to parse as JSON, which a
// plain word like a task id or "paused" never does, but is worth guarding
// rather than assuming.
func asString(v any) string {
	s, _ := v.(string)
	return s
}
