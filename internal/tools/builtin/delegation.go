package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/google/uuid"
)

// Background delegations that survive the daemon.
//
// claude_code.call with background=true runs a separate Claude Code process
// and announces its result as a delegation.completed event. That process
// lives in the daemon's cgroup and dies with it, and so did the goroutine
// waiting on it — so a restart lost the work and its result, and the request
// it served was never answered.
//
// Each delegation is now a row before it starts (tasks, status "delegated"):
// the session id it runs under, minted up front so it can be resumed by id;
// its input; and whose request it serves. Its end is announced under an id
// derived from the job (the bus keeps one event per id), and only then is the
// row closed. A restart resumes every row still open: the same session when
// its transcript exists, told it was interrupted, or the same work afresh when
// the process died before writing any.

// DelegationOrigin is the request a delegation serves.
type DelegationOrigin struct {
	RequestID, ChannelID, Target, Where string
}

// freshSessionKey carries a session id minted for a session that does not
// exist yet; run() starts it under that id instead of resuming it.
const freshSessionKey = "_fresh_session_id"

// delegationRowID is a delegation's journal row.
func delegationRowID(jobID string) string { return "delegation-" + jobID }

// DelegationEventID is the id a delegation's end is announced under: the
// same for every attempt, so announcing it twice is announcing it once.
func DelegationEventID(jobID string) string { return "delegation-done-" + jobID }

// pinSession fixes the session a delegation runs under before it starts, so
// the journal can name it: the caller's, the prior session this one would
// have resumed, or a new one minted now.
func (t *ClaudeCodeTool) pinSession(input map[string]any, prompt string) map[string]any {
	out := make(map[string]any, len(input)+1)
	for k, v := range input {
		out[k] = v
	}
	if sid, _ := out["session_id"].(string); strings.TrimSpace(sid) != "" {
		return out
	}
	if eph, _ := out["ephemeral"].(bool); !eph {
		if reusable := findReusableCodingSession(t.Store, t.AgentID, "claude_code", prompt); reusable != nil {
			out["session_id"] = reusable.SessionID
			return out
		}
	}
	out[freshSessionKey] = uuid.New().String()
	return out
}

// sessionOf is the session a pinned input runs under.
func sessionOf(input map[string]any) string {
	if sid, _ := input["session_id"].(string); sid != "" {
		return sid
	}
	sid, _ := input[freshSessionKey].(string)
	return sid
}

// journalDelegation writes the delegation's row before it starts.
func (t *ClaudeCodeTool) journalDelegation(jobID string, input map[string]any, prompt string) {
	if t.Store == nil {
		return
	}
	var origin DelegationOrigin
	if t.Origin != nil {
		origin = t.Origin()
	}
	raw, _ := json.Marshal(input)
	workdir, _ := input["working_dir"].(string)
	if _, err := t.Store.CreateTask(store.Task{
		ID: delegationRowID(jobID), AgentID: t.AgentID, Status: store.TaskDelegated,
		Title: "Delegated: " + truncate(firstLine(prompt), 100), Goal: prompt,
		SessionKey: sessionOf(input), Workdir: workdir, Progress: string(raw),
		RequestID: origin.RequestID, ChannelID: origin.ChannelID, Target: origin.Target, Context: origin.Where,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "karmax: could not journal delegation %s; it will not survive a restart: %v\n", jobID, err)
	}
}

// runDelegation runs a journalled delegation to its end and announces it.
func (t *ClaudeCodeTool) runDelegation(jobID string, input map[string]any, prompt string) {
	// Not the caller's context: it dies with the turn, which is the whole
	// point of running this after the turn is over.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	res, err := t.run(ctx, input, prompt)

	task := prompt
	payload := map[string]any{"job_id": jobID, "tool": "claude_code.call"}
	if t.Store != nil {
		if row, found, _ := t.Store.GetTask(delegationRowID(jobID)); found {
			task = row.Goal
			if row.RequestID != "" || row.Target != "" {
				payload["origin_request"] = row.RequestID
				payload["origin_channel_id"] = row.ChannelID
				payload["origin_target"] = row.Target
				payload["origin_where"] = row.Context
			}
		}
	}
	payload["task"] = truncate(task, 500)
	status, output := "completed", ""
	switch {
	case err != nil:
		status, output = "failed", err.Error()
		payload["error"] = output
	case res.IsError:
		status, output = "failed", res.Error
		payload["error"] = output
	default:
		if out, ok := res.Output.(map[string]any); ok {
			payload["output"] = out["output"]
			payload["session_id"] = out["session_id"]
			output, _ = out["output"].(string)
		}
	}
	payload["status"] = status
	if t.Publish == nil {
		return
	}
	evt := bus.NewEvent(bus.EventDelegationDone, t.AgentID, payload)
	evt.ID = DelegationEventID(jobID)
	if err := t.Publish(evt); err != nil {
		// The work happened; only the announcement failed. The row stays open,
		// so the next start announces it rather than nobody ever hearing.
		fmt.Fprintf(os.Stderr, "karmax: background delegation %s finished but could not be announced: %v\n", jobID, err)
		return
	}
	t.closeDelegation(jobID, status, output)
}

// closeDelegation records a delegation's end, once it has been announced.
func (t *ClaudeCodeTool) closeDelegation(jobID, status, output string) {
	if t.Store == nil {
		return
	}
	final := store.TaskDone
	if status == "failed" {
		final = store.TaskFailed
	}
	_ = t.Store.UpdateTask(delegationRowID(jobID), store.TaskUpdate{
		Status: final, Reported: truncate(strings.TrimSpace(output), 2000), KeepNextActionAt: true,
	})
}

// interruptedPrompt is what a delegation resumed in its own session is told.
func interruptedPrompt(prompt string) string {
	return "KARMAX restarted while you were working on this, and your process was stopped mid-way. " +
		"Your work so far is in this session and on disk. Check what is already done, then continue " +
		"from there and finish — do not start over or repeat anything that already happened " +
		"(messages sent, files written, commands with effects):\n\n" + prompt
}

// ResumeDelegations restarts every delegation the daemon died during, each as
// base would have run it for its agent. One whose end was announced before
// the daemon died is only closed. Returns how many were resumed.
func ResumeDelegations(base ClaudeCodeTool) int {
	if base.Store == nil {
		return 0
	}
	rows, err := base.Store.ListTasks(store.TaskDelegated, 100)
	if err != nil {
		fmt.Fprintf(os.Stderr, "karmax: could not read interrupted delegations: %v\n", err)
		return 0
	}
	n := 0
	for _, row := range rows {
		jobID := strings.TrimPrefix(row.ID, "delegation-")
		t := base
		t.AgentID = row.AgentID
		if base.Store.EventLogged(DelegationEventID(jobID)) {
			t.closeDelegation(jobID, "completed", row.Reported)
			continue
		}
		var input map[string]any
		if err := json.Unmarshal([]byte(row.Progress), &input); err != nil || input == nil {
			input = map[string]any{"prompt": row.Goal}
		}
		prompt := row.Goal
		if sid := row.SessionKey; looksLikeUUID(sid) && sessionCreated(sid) {
			// Its transcript exists: the same session goes on, told why.
			delete(input, freshSessionKey)
			input["session_id"] = sid
			prompt = interruptedPrompt(row.Goal)
		} else if looksLikeUUID(sid) {
			// It died before writing anything: the same work, under the same id.
			delete(input, "session_id")
			input[freshSessionKey] = sid
		}
		go t.runDelegation(jobID, input, prompt)
		n++
	}
	return n
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
