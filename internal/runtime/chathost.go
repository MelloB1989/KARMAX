package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/MelloB1989/karmax/internal/api"
	"github.com/MelloB1989/karmax/internal/browser"
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
	// One dedupe set per turn, closed over by OnEvent below: a tool_update
	// can repeat for the same call, and a ticket must not.
	seenTickets := map[string]bool{}
	turn, err := rt.harness.SendWith(ctx, "chat:"+id, "chat", message, harness.Options{
		// Where chatlog reads. Left to the supervisor's default this lands in
		// a per-conversation directory nothing ever lists.
		Workdir:   hostpaths.WorkDir(),
		SessionID: id,
		// Empty when the browser is closed — the normal case, not a failure.
		MCPConfig: browserMCPConfig(ctx, browser.Shared(rt.cfg.Karmax.DataDir), "chat"),
		PluginDir: harnessPluginDir("chat", rt.skillsDir),
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
			// A background delegation's id is only ever in its RESULT, never
			// its input — see ticketFrom — so the ticket can only be built
			// from the stream, while the tool's real output is still here.
			if tk, ok := ticketFrom(e, seenTickets); ok {
				onEvent(tk)
			}
		},
	})
	if err != nil {
		return "", err
	}
	// The footer's facts, announced before `done`: they only exist once the
	// turn has finished.
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

// ticketPayload is every field a background job's own JSON result might use
// for its id and its label. task.start, claude_code.call --background and
// sandbox.start each pick a different pair of names, and all three arrive
// under harness tool names that have nothing to do with any of them (Bash,
// mostly) — so the id is matched on the JSON, never on which tool carried it.
type ticketPayload struct {
	TaskID string `json:"task_id"`
	JobID  string `json:"job_id"`
	RunID  string `json:"run_id"`
	Goal   string `json:"goal"`
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
}

// ticketFrom turns one streaming tool_update into a ticket, if the tool's own
// result carries a background job's id. seen is the calling turn's dedupe
// set, keyed by e.Tool.ID: a tool_update can repeat for the same call, and a
// ticket must not.
//
// OnEvent — chatTurn's only caller — is invoked synchronously from
// Session.Send's own read loop, one event at a time, under the session's
// mutex (see session.go: "Exactly one turn may be in flight at a time" and
// Send's `select` over s.events calling emit(sink, ev) inline). So seen is
// never touched concurrently and needs no lock of its own.
func ticketFrom(e harness.Event, seen map[string]bool) (api.ChatEvent, bool) {
	if e.Kind != harness.KindToolUpdate || e.Tool == nil {
		return api.ChatEvent{}, false
	}
	t := e.Tool
	if t.Status != harness.StatusCompleted && t.Status != harness.StatusFailed {
		return api.ChatEvent{}, false // not a terminal update
	}
	if t.Output == "" || seen[t.ID] {
		return api.ChatEvent{}, false
	}
	id, label := parseTicketPayload(t.Output)
	if id == "" {
		return api.ChatEvent{}, false
	}
	seen[t.ID] = true
	if label == "" {
		label = t.Title // never an empty card
	}
	return api.ChatEvent{Kind: "ticket", JobID: id, Text: trimTo(label, 80)}, true
}

// parseTicketPayload looks for a background job's id and label inside a
// tool's output. It tolerates text around the JSON object (a tool may print
// a sentence and then a result) by decoding from the first '{', and it
// tolerates truncation: truncateOutput can only cut a result short, never
// corrupt what it keeps, and json.Decoder simply errors on the cut — which
// this reports as "no ticket", never as an error or a panic.
func parseTicketPayload(output string) (id, label string) {
	i := strings.IndexByte(output, '{')
	if i < 0 {
		return "", ""
	}
	var p ticketPayload
	if err := json.NewDecoder(strings.NewReader(output[i:])).Decode(&p); err != nil {
		return "", ""
	}
	id = firstNonEmpty(p.TaskID, p.JobID, p.RunID)
	if id == "" {
		return "", ""
	}
	return id, firstNonEmpty(p.Goal, p.Title, p.Prompt)
}

// firstNonEmpty returns the first non-empty value, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
