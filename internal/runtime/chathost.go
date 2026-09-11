package runtime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/MelloB1989/karmax/internal/api"
	"github.com/MelloB1989/karmax/internal/harness"
)

// errChatHarnessUnavailable mirrors the message the API layer used to return
// itself, before the harness field moved out of api.Server.
var errChatHarnessUnavailable = errors.New("the brain is not running")

// chatTurn is wired to api.Server.SetChatTurn (see Start, beside SetRunLoop).
// It is where a harness.Event becomes the api.ChatEvent the streaming
// endpoint forwards — internal/api must not import internal/harness, so this
// adapter is the only place the two types meet.
func (rt *KarmaxRuntime) chatTurn(ctx context.Context, key, message string, onEvent func(api.ChatEvent)) (string, error) {
	if rt.harness == nil {
		return "", errChatHarnessUnavailable
	}
	turn, err := rt.harness.SendWith(ctx, key, "chat", message, harness.Options{
		OnEvent: func(e harness.Event) {
			onEvent(api.ChatEvent{Kind: e.Kind, Text: e.Text, Tool: e.Tool, Phase: e.Phase, JobID: e.JobID})
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
	return turn.Text, nil
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
