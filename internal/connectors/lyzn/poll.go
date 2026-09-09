// The poll: the only thing in this package that runs on its own.
package lyzn

import (
	"context"
	"encoding/json"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

const (
	// pollID is the second half of the stored cursor key.
	pollID = "work"

	// EventTaskApproved is raised once per approved task. A constant because a
	// recipe waits on the string, and a typo in one of two copies is a loop
	// that never runs.
	EventTaskApproved = "lyzn.task.approved"

	// pollInterval is a minute because LYZN calls a machine asleep after
	// three missed thirty-second beats, and a laptop that is working
	// perfectly should not be drawn as gone on somebody's phone. The host
	// floors anything shorter at a minute anyway.
	pollInterval = time.Minute

	// seenLimit bounds the cursor. It holds the ids currently on the queue,
	// and a queue longer than this is not a situation more ids would help.
	seenLimit = 200
)

func (c *Connector) Sources() []connectorkit.EventSource {
	return []connectorkit.EventSource{{
		ID:        pollID,
		Kind:      connectorkit.SourcePoll,
		EventKind: EventTaskApproved,
		Interval:  pollInterval,
		Poll:      pollWork,
	}}
}

// pollWork beats, then asks what is waiting, then says what is new.
//
// **The cursor is the set of task ids that were on the queue last time**, not
// a timestamp. A timestamp is the obvious choice and it is wrong here: a task
// carries the moment the conversation happened, not the moment somebody
// approved it, so a promise made yesterday and approved this morning is older
// than the cursor and would never be raised at all.
//
// Holding the queue itself has a second property worth having. A task leaves
// the queue when it is claimed and comes back if the claim's lease runs out —
// so it drops out of the cursor by itself, and a task that was stranded is
// announced again, which is exactly what should happen to work nobody
// finished.
func pollWork(ctx context.Context, cr connectorkit.Credentials, cursor string) ([]map[string]any, string, error) {
	// The beat is the cheap question and it answers two: it keeps this
	// machine present in the app, and it says whether the second request is
	// worth making at all. On the overwhelming majority of ticks there is no
	// work and this is the only call.
	state, err := beat(ctx, cr)
	if err != nil {
		return nil, cursor, err
	}
	if state.Tasks == 0 {
		// Nothing waiting means nothing seen: a task approved after this
		// tick is new whatever it was called before.
		return nil, "", nil
	}

	tasks, err := waiting(ctx, cr)
	if err != nil {
		return nil, cursor, err
	}

	seen := decodeSeen(cursor)
	events := make([]map[string]any, 0, len(tasks))
	next := make([]string, 0, len(tasks))
	for _, task := range tasks {
		next = append(next, task.TaskID)
		if seen[task.TaskID] {
			continue
		}
		events = append(events, event(task))
	}

	return events, encodeSeen(next), nil
}

// event is one task as the bus carries it.
//
// The key names are chosen, not incidental. KARMAX fences free text by field
// name before it reaches a log or a model — `text`, `title`, `summary` and
// `comment` are on that list — and every string in here that a person said
// rather than a program computed is carried under one of them. The quote is
// `comment` for exactly that reason: it is a line of somebody's speech, and
// speech is the one thing in this payload that could try to give an
// instruction.
func event(task work) map[string]any {
	facts := make([]string, 0, len(task.Context.Facts))
	for _, f := range task.Context.Facts {
		facts = append(facts, f.Text)
	}
	return map[string]any{
		"task_id":      task.TaskID,
		"text":         task.Text,
		"kind":         task.Kind,
		"comment":      task.Quote,
		"due_at":       task.DueAt,
		"recording_id": task.RecordingID,
		"created_at":   task.CreatedAt,
		"title":        task.Context.Title,
		"summary":      task.Context.Summary,
		"facts":        facts,
	}
}

// decodeSeen reads the cursor. An unreadable one is treated as empty, which
// re-announces the queue once rather than going silent — the safe direction
// for a cursor whose whole job is suppression.
func decodeSeen(cursor string) map[string]bool {
	seen := map[string]bool{}
	if cursor == "" {
		return seen
	}
	var ids []string
	if err := json.Unmarshal([]byte(cursor), &ids); err != nil {
		return seen
	}
	for _, id := range ids {
		seen[id] = true
	}
	return seen
}

func encodeSeen(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	if len(ids) > seenLimit {
		ids = ids[:seenLimit]
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return ""
	}
	return string(raw)
}
