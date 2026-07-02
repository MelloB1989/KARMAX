package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/google/uuid"
)

type SchedulerTool struct {
	Scheduler *scheduler.Scheduler
	AgentID   string
}

func (t *SchedulerTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "scheduler.add",
		Description: "Schedule a task to run at a future time. Use this when the user asks you to do something later, in N minutes, at a specific time, or on a recurring schedule. The payload will be delivered back to you as an event.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Short name for the scheduled task"},
				"delay_minutes": {"type": "integer", "description": "Run after this many minutes from now (for one-shot delays). Use this OR cron, not both."},
				"cron": {"type": "string", "description": "Cron expression for recurring tasks (e.g. '*/5 * * * *' for every 5 mins, '0 9 * * *' for daily at 9am). Use this OR delay_minutes, not both."},
				"payload": {"type": "string", "description": "JSON payload describing what to do when the task fires. This will be delivered back to you as an event."}
			},
			"required": ["name", "payload"]
		}`),
	}
}

func (t *SchedulerTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	name, _ := input["name"].(string)
	payloadStr, _ := input["payload"].(string)
	delayMin, _ := input["delay_minutes"].(float64)
	cronExpr, _ := input["cron"].(string)

	if name == "" {
		return tools.ToolResult{IsError: true, Error: "name is required"}, nil
	}

	var payload map[string]any
	if payloadStr != "" {
		json.Unmarshal([]byte(payloadStr), &payload)
	}
	if payload == nil {
		payload = map[string]any{"task": name}
	}

	// AgentID is bound per-agent (bindAgentTools). An unbound instance would
	// schedule jobs that fire to a nonexistent agent and vanish, so refuse.
	agentID := t.AgentID
	if agentID == "" {
		return tools.ToolResult{IsError: true, Error: "scheduler tool is not bound to an agent"}, nil
	}

	job := scheduler.ScheduledJob{
		ID:      uuid.New().String(),
		Name:    name,
		AgentID: agentID,
		Payload: payload,
		Enabled: true,
	}

	if cronExpr != "" {
		job.Cron = cronExpr
	} else if delayMin > 0 {
		fireAt := time.Now().Add(time.Duration(delayMin) * time.Minute)
		job.Cron = fmt.Sprintf("%d %d %d %d *", fireAt.Minute(), fireAt.Hour(), fireAt.Day(), int(fireAt.Month()))
	} else {
		return tools.ToolResult{IsError: true, Error: "either delay_minutes or cron is required"}, nil
	}

	if err := t.Scheduler.AddJob(job); err != nil {
		return tools.ToolResult{IsError: true, Error: fmt.Sprintf("failed to schedule: %v", err)}, nil
	}

	result := map[string]any{
		"job_id":  job.ID,
		"name":    job.Name,
		"cron":    job.Cron,
		"status":  "scheduled",
		"message": fmt.Sprintf("Task '%s' scheduled successfully", name),
	}

	return tools.ToolResult{Output: result}, nil
}
