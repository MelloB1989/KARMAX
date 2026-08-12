package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/clock"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/google/uuid"
)

// Waking itself up later.
//
// reminder.add is a false friend: it writes a device action for the operator's
// phone and never reaches the agent at all. scheduler.add does fire back, but it
// builds a cron expression even for "in twenty minutes" — pinning minute, hour,
// day and month, so a one-shot silently becomes an annually recurring job that
// nothing ever deletes.
//
// The timers table has been the right primitive the whole time: exactly-once
// firing, a sweep that runs immediately at startup so anything due during
// downtime still fires, and no recurrence to clean up. It was only ever exposed
// to loops.

// SelfRemindTool arms a durable timer that wakes the agent with a prompt.
type SelfRemindTool struct {
	Clock   *clock.Clock
	AgentID string
}

func (t *SelfRemindTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "self.remind",
		Description: "Wake YOURSELF later with an instruction — the note comes back to you, not to the operator's phone. " +
			"Use it to follow up on something you are waiting for: 'in 2h, check whether Siva replied about the APK'. " +
			"It survives a restart and fires once. For a reminder the OPERATOR should see on their phone, use reminder.add instead; " +
			"for something recurring, write a recipe.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"prompt": {"type": "string", "description": "What you should do when it fires. Write it for yourself with no memory of now: name the person, chat or task."},
				"in": {"type": "string", "description": "Delay from now, e.g. '20m', '2h', '36h'."},
				"at": {"type": "string", "description": "Absolute RFC3339 time, e.g. '2026-08-14T09:00:00+05:30'. Use this OR 'in'."}
			},
			"required": ["prompt"]
		}`),
	}
}

func (t *SelfRemindTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	if t.Clock == nil {
		return tools.ErrorResult(fmt.Errorf("timers are not available on this instance")), nil
	}
	prompt, _ := input["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return tools.ErrorResult(fmt.Errorf("say what you should do when it fires")), nil
	}

	fireAt, err := whenToFire(input)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if time.Until(fireAt) < time.Second {
		return tools.ErrorResult(fmt.Errorf("%s is not in the future", fireAt.Format(time.RFC3339))), nil
	}

	// The id is ours, not the caller's: re-arming an existing id moves that
	// deadline instead of adding one, and two unrelated reminders must not
	// collide into a single timer.
	id := "self-remind:" + uuid.New().String()
	if err := t.Clock.Arm(store.Timer{
		ID:      id,
		FireAt:  fireAt,
		AgentID: t.AgentID,
		// The prompt rides in the payload so the turn it wakes reads as an
		// instruction rather than as raw machine state — extractLoopPrompt
		// picks "prompt" up and renders it as the task.
		Payload: map[string]any{"prompt": prompt, "source": "self.remind"},
	}); err != nil {
		return tools.ErrorResult(fmt.Errorf("could not arm the timer: %w", err)), nil
	}
	return tools.SuccessResult(map[string]any{
		"status":   "armed",
		"timer_id": id,
		"fires_at": fireAt.Format(time.RFC3339),
		"in":       time.Until(fireAt).Round(time.Minute).String(),
	}), nil
}

// whenToFire reads either a delay or an absolute time.
func whenToFire(input map[string]any) (time.Time, error) {
	if at, _ := input["at"].(string); strings.TrimSpace(at) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(at))
		if err != nil {
			return time.Time{}, fmt.Errorf("could not read %q as a time: use RFC3339 like 2026-08-14T09:00:00+05:30", at)
		}
		return parsed, nil
	}
	in, _ := input["in"].(string)
	in = strings.TrimSpace(in)
	if in == "" {
		return time.Time{}, fmt.Errorf("give either 'in' (e.g. '2h') or 'at' (RFC3339)")
	}
	d, err := time.ParseDuration(in)
	if err != nil {
		return time.Time{}, fmt.Errorf("could not read %q as a duration: use forms like '20m', '2h', '36h'", in)
	}
	return time.Now().Add(d), nil
}
