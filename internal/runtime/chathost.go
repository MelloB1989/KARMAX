package runtime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/MelloB1989/karmax/internal/api"
	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/hostpaths"
)

// errChatHarnessUnavailable mirrors the message the API layer used to return
// itself, before the harness field moved out of api.Server.
var errChatHarnessUnavailable = errors.New("the brain is not running")

// chatTurn is wired to api.Server.SetChatTurn (see Start, beside SetRunLoop).
// It is where a harness.Event becomes the api.ChatEvent the streaming
// endpoint forwards — internal/api must not import internal/harness, so this
// adapter is the only place the two types meet.
// The argument is the CONVERSATION id, not the supervisor key. It is also the
// harness session id and the name of the transcript on disk: the three were
// three different values once, and the result was a chat that worked and a
// conversation that could never be listed, reopened or deleted.
func (rt *KarmaxRuntime) chatTurn(ctx context.Context, id, message string, onEvent func(api.ChatEvent)) (string, error) {
	if rt.harness == nil {
		return "", errChatHarnessUnavailable
	}
	turn, err := rt.harness.SendWith(ctx, "chat:"+id, "chat", message, harness.Options{
		// Where chatlog reads. Left to the supervisor's default this lands in
		// a per-conversation directory nothing ever lists.
		Workdir:   hostpaths.WorkDir(),
		SessionID: id,
		OnEvent: func(e harness.Event) {
			ev := api.ChatEvent{Kind: string(e.Kind), Text: e.Text, JobID: e.JobID}
			if e.Tool != nil {
				locs := make([]api.ChatLocation, 0, len(e.Tool.Locations))
				for _, l := range e.Tool.Locations {
					locs = append(locs, api.ChatLocation{Path: l.Path, Line: l.Line})
				}
				ev.Tool = &api.ChatTool{
					ID:        e.Tool.ID,
					Title:     e.Tool.Title,
					Kind:      apiToolKind(e.Tool.Kind),
					Status:    apiToolStatus(e.Tool.Status),
					Locations: locs,
					Output:    e.Tool.Output,
				}
			}
			ev.Plan = apiChatPlan(e.Plan)
			onEvent(ev)
		},
	})
	if err != nil {
		return "", err
	}
	// Background jobs are announced from the finished turn, before `done`,
	// because their ids only exist once the tool has returned.
	for _, t := range chatTickets(turn) {
		onEvent(t)
	}
	// The footer's facts, announced before `done` for the same reason tickets
	// are: they only exist once the turn has finished.
	onEvent(api.ChatEvent{
		Kind:       "meta",
		Model:      turn.Model,
		DurationMS: turn.Duration.Milliseconds(),
		CostUSD:    turn.CostUSD,
	})
	return turn.Text, nil
}

// apiToolKind coerces a kind to one of the ten values ACP defines. Today the
// harness only ever produces those ten, but an ACP client is a provider's own
// vocabulary flowing through unchecked — a stray "browse" would pass
// json.Marshal here and violate the TypeScript client's closed union silently
// on the far end.
func apiToolKind(k harness.ToolKind) string {
	switch k {
	case harness.ToolRead, harness.ToolEdit, harness.ToolDelete, harness.ToolMove,
		harness.ToolSearch, harness.ToolExecute, harness.ToolThink, harness.ToolFetch,
		harness.ToolSwitchMode, harness.ToolOther:
		return string(k)
	default:
		return string(harness.ToolOther)
	}
}

// apiChatPlan is built non-nil even for an empty plan: a nil slice marshals
// to JSON null, and the TypeScript side declares plan non-nullable and reads
// plan.length unguarded — a null here doesn't fail to parse, it throws inside
// deriveRows and takes the whole transcript down with it.
func apiChatPlan(entries []harness.PlanEntry) []api.ChatPlanEntry {
	out := make([]api.ChatPlanEntry, 0, len(entries))
	for _, p := range entries {
		out = append(out, api.ChatPlanEntry{
			Content: p.Content, Status: p.Status,
			ActiveForm: p.ActiveForm, Priority: p.Priority,
		})
	}
	return out
}

// apiToolStatus coerces a status to one of the four values ACP defines. An
// unrecognised status means we do not know the call succeeded, and reporting
// it as "completed" would be a worse lie than reporting it as "failed".
func apiToolStatus(s harness.Status) string {
	switch s {
	case harness.StatusPending, harness.StatusInProgress, harness.StatusCompleted, harness.StatusFailed:
		return string(s)
	default:
		return string(harness.StatusFailed)
	}
}

// chatTickets reports the background jobs a finished turn started.
//
// Not a harness event, because the harness cannot know: a delegation's job id
// is in the tool's RESULT, and the session loop only reads assistant messages
// on its way to a Turn. Reading the completed turn's tool calls is both
// simpler and correct — a card for background work can only be useful after
// the turn ends, and the turn ends quickly precisely because the work went to
// the background.
func chatTickets(turn harness.Turn) []api.ChatEvent {
	var out []api.ChatEvent
	for _, tc := range turn.ToolCalls {
		if tc.Name != "claude_code.call" && tc.Name != "codex.call" {
			continue
		}
		var in struct {
			Background bool   `json:"background"`
			Prompt     string `json:"prompt"`
			JobID      string `json:"job_id"`
		}
		if json.Unmarshal(tc.Input, &in) != nil || !in.Background {
			continue
		}
		out = append(out, api.ChatEvent{Kind: "ticket", JobID: in.JobID, Text: trimTo(in.Prompt, 80)})
	}
	return out
}
