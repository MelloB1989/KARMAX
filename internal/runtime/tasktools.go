package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
)

// The task surface.
//
// One tool to take ownership of something, one to see what is owned, one to
// close it. The runner does the work; these are how the orchestrator hands work
// over to itself and how anybody finds out what is outstanding.

type taskCreateTool struct {
	ref     *harnessRef
	agentID string
}

func (t *taskCreateTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "task.create",
		Description: "Take ownership of work that will not finish in this turn. " +
			"KARMAX then keeps at it in its own session — across restarts, retrying its own " +
			"failures — and messages the operator when it is done, when it is genuinely stuck, " +
			"or when something happens they need to know. " +
			"Use this INSTEAD of promising to do something later: a promise ends with the turn, " +
			"a task does not. Anything multi-step, anything that has to wait on something, " +
			"anything you cannot finish right now.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"goal":{"type":"string","description":"What must be true for this to be finished, in full. Include everything the work needs — repo, names, constraints, acceptance criteria — because the session working on it starts from this and not from the current conversation."},
				"title":{"type":"string","description":"Short label for status lists. Defaults to the first line of the goal."},
				"workdir":{"type":"string","description":"Directory the work happens in, e.g. a repo checkout. Optional."},
				"channel_id":{"type":"string","description":"KARMAX channel to report back on. Defaults to the one this conversation is on."},
				"target":{"type":"string","description":"Chat/thread to report back to. Defaults to the current one."},
				"start_in_minutes":{"type":"number","description":"Wait this long before the first round. Omit to start immediately."}
			},
			"required":["goal"]
		}`),
	}
}

func (t *taskCreateTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	rt := t.ref.get()
	if rt == nil {
		return tools.ErrorResult(fmt.Errorf("the runtime is not ready")), nil
	}
	goal, _ := in["goal"].(string)
	if strings.TrimSpace(goal) == "" {
		return tools.ErrorResult(fmt.Errorf("a task needs a goal — what must be true for it to be finished")), nil
	}
	title, _ := in["title"].(string)
	workdir, _ := in["workdir"].(string)
	channelID, _ := in["channel_id"].(string)
	target, _ := in["target"].(string)

	task := store.Task{
		AgentID: t.agentID, Title: strings.TrimSpace(title), Goal: goal,
		Workdir:   strings.TrimSpace(workdir),
		ChannelID: strings.TrimSpace(channelID), Target: strings.TrimSpace(target),
	}
	if mins, ok := in["start_in_minutes"].(float64); ok && mins > 0 {
		at := time.Now().Add(time.Duration(mins) * time.Minute)
		task.NextActionAt = &at
	}

	created, err := rt.store.CreateTask(task)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if rt.harness == nil {
		// Recorded either way — the work is not lost — but the operator should
		// know nothing will pick it up.
		return tools.SuccessResult(map[string]any{
			"task_id": created.ID, "title": created.Title, "status": "recorded",
			"warning": "the harness is disabled, so nothing will work this task until it is enabled",
		}), nil
	}
	return tools.SuccessResult(map[string]any{
		"task_id": created.ID,
		"title":   created.Title,
		"status":  created.Status,
		"note": "Owned. KARMAX will work it in its own session until it is done and report back. " +
			"Tell the operator it is underway — do NOT claim any part of it is finished yet.",
	}), nil
}

type taskListTool struct{ ref *harnessRef }

func (t *taskListTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "task.list",
		Description: "What KARMAX is currently working on, and what it has finished. " +
			"Check here before telling the operator something is or is not underway.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"status":{"type":"string","description":"live (default: everything unfinished), open, working, blocked, done, failed, or all."},
				"limit":{"type":"integer","description":"default 20"}
			}
		}`),
	}
}

func (t *taskListTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	rt := t.ref.get()
	if rt == nil {
		return tools.ErrorResult(fmt.Errorf("the runtime is not ready")), nil
	}
	status, _ := in["status"].(string)
	if strings.TrimSpace(status) == "" {
		status = "live"
	}
	if status == "all" {
		status = ""
	}
	limit := 20
	if n, ok := in["limit"].(float64); ok && n > 0 {
		limit = int(n)
	}
	rows, err := rt.store.ListTasks(status, limit)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		item := map[string]any{
			"task_id": r.ID, "title": r.Title, "status": r.Status,
			"rounds": r.Attempts, "updated": r.UpdatedAt.Format(time.RFC3339),
		}
		if r.LastError != "" {
			item["last_error"] = r.LastError
		}
		if r.Reported != "" {
			item["last_told_operator"] = r.Reported
		}
		out = append(out, item)
	}
	return tools.SuccessResult(map[string]any{"tasks": out, "count": len(out)}), nil
}

type taskUpdateTool struct{ ref *harnessRef }

func (t *taskUpdateTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "task.update",
		Description: "Change a task: close it, cancel it, or unblock it after the operator has " +
			"answered. Setting a blocked task back to working is what resumes it — the runner " +
			"leaves blocked tasks alone until somebody does that.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"task_id":{"type":"string","description":"From task.list or task.create."},
				"status":{"type":"string","description":"working | blocked | done | failed."},
				"note":{"type":"string","description":"What changed, recorded for the next round to read."}
			},
			"required":["task_id","status"]
		}`),
	}
}

func (t *taskUpdateTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	rt := t.ref.get()
	if rt == nil {
		return tools.ErrorResult(fmt.Errorf("the runtime is not ready")), nil
	}
	id, _ := in["task_id"].(string)
	status, _ := in["status"].(string)
	note, _ := in["note"].(string)
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case store.TaskWorking, store.TaskBlocked, store.TaskDone, store.TaskFailed:
	default:
		return tools.ErrorResult(fmt.Errorf("status must be working, blocked, done or failed")), nil
	}
	task, found, err := rt.store.GetTask(strings.TrimSpace(id))
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if !found {
		return tools.ErrorResult(fmt.Errorf("no task with id %s", id)), nil
	}

	u := store.TaskUpdate{Status: status}
	if strings.TrimSpace(note) != "" {
		u.Progress = strings.TrimSpace(task.Progress + "\n\n[operator] " + note)
	}
	if status == store.TaskWorking {
		// Due now: the point of unblocking is that it carries on.
		now := time.Now()
		u.NextActionAt = &now
	}
	if err := rt.store.UpdateTask(task.ID, u); err != nil {
		return tools.ErrorResult(err), nil
	}
	return tools.SuccessResult(map[string]any{
		"task_id": task.ID, "title": task.Title, "status": status,
	}), nil
}

// agentIDOf names the agent tasks are filed under. The first configured agent
// is the orchestrator; no agents at all is still valid and simply files them
// unattributed rather than failing.
func agentIDOf(cfg *config.KarmaxConfig) string {
	if cfg == nil || len(cfg.Agents) == 0 {
		return ""
	}
	return cfg.Agents[0].ID
}
