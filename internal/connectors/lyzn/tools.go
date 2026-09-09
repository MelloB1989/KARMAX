// The four verbs, in the order they are meant to be used.
package lyzn

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Tools is deliberately four calls and not fourteen.
//
// There is one thing to do here and it is a sequence: see what was approved,
// take one, do it with whatever this machine already has, say what happened.
// Anything else LYZN can do belongs to the person holding the phone — an agent
// cannot approve its own work, and there is no call here that would let it
// try.
func (c *Connector) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{
		{
			Name: "lyzn.work.list",
			Description: "List the commitments the operator approved in the LYZN app and that no machine has claimed yet, oldest first. " +
				"Each one carries the conversation it was promised in — the title, the summary, the exact words said — because a promise is rarely actionable without them. " +
				"Claim one with lyzn.work.claim before doing it.",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
			Call:       listWork,
		},
		{
			Name: "lyzn.work.claim",
			Description: "Take one approved task, so no other machine runs it too. " +
				"The claim is a fifteen-minute loan: finish and report inside it, or LYZN puts the task back for someone else. " +
				"Re-claiming a task this machine already holds is fine and is how a restarted run picks up where it stopped. " +
				"Returns the task and the conversation it came from.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"task_id":{"type":"string","description":"The taskId from lyzn.work.list."}
				},
				"required":["task_id"]
			}`),
			Call: claimWork,
		},
		{
			Name: "lyzn.work.report",
			Description: "Say what happened to a claimed task. LYZN closes it and prints a receipt the operator sees on their phone. " +
				"Report honestly: 'done' prints a receipt saying the promise was kept, so use 'blocked' or 'failed' when it was not, and say why in the summary — a receipt for work nobody did is worse than no receipt. " +
				"Posting the same result twice is safe; the first receipt is what comes back.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"task_id":{"type":"string","description":"The task that was claimed."},
					"outcome":{"type":"string","enum":["done","blocked","failed"],"description":"done only if the thing promised actually happened."},
					"summary":{"type":"string","description":"One or two sentences a person will read on a receipt: what was done, or what stopped it."},
					"artifacts":{
						"type":"array",
						"description":"What the work produced — a file, a pull request, a message. Optional.",
						"items":{
							"type":"object",
							"properties":{
								"name":{"type":"string"},
								"uri":{"type":"string"}
							},
							"required":["name"]
						}
					}
				},
				"required":["task_id","outcome","summary"]
			}`),
			Call: reportWork,
		},
		{
			Name:        "lyzn.status",
			Description: "Whether this machine is paired with LYZN and how much approved work is waiting. Also tells LYZN the machine is awake.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			Call:        status,
		},
	}
}

func listWork(ctx context.Context, cr connectorkit.Credentials, _ map[string]any) (any, error) {
	tasks, err := waiting(ctx, cr)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, event(task, root(cr)))
	}
	return map[string]any{"tasks": out, "count": len(out)}, nil
}

func claimWork(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	id := strings.TrimSpace(strArg(in, "task_id"))
	if id == "" {
		return nil, fmt.Errorf("lyzn: task_id is required — take it from lyzn.work.list")
	}
	task, err := claim(ctx, cr, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"task":  event(task, root(cr)),
		"lease": "15m",
		"next":  "Do the work, then call lyzn.work.report with this task_id.",
	}, nil
}

func reportWork(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	id := strings.TrimSpace(strArg(in, "task_id"))
	if id == "" {
		return nil, fmt.Errorf("lyzn: task_id is required — report against the task that was claimed")
	}
	said := strings.ToLower(strings.TrimSpace(strArg(in, "outcome")))
	summary := strings.TrimSpace(strArg(in, "summary"))
	if summary == "" {
		return nil, fmt.Errorf("lyzn: summary is required — it is the line a person reads on the receipt")
	}

	// LYZN's wire words are "success" and "failure"; the model's are the
	// three a coding harness reports. Only one of them prints a kept promise,
	// and anything unrecognised is a failure rather than a guess: a receipt
	// wrongly saying "done" is the one mistake this connector must not make.
	outcome := outcomeFailure
	switch said {
	case "done", "success", "completed":
		outcome = outcomeSuccess
	case "blocked", "failed", "failure", "":
		outcome = outcomeFailure
	default:
		outcome = outcomeFailure
		summary = "reported as " + said + ": " + summary
	}

	body := map[string]any{
		"outcome":    outcome,
		"summary":    summary,
		"finishedAt": nowISO(),
	}
	if artifacts := artifactsArg(in); len(artifacts) > 0 {
		body["artifacts"] = artifacts
	}

	out, err := report(ctx, cr, id, body)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func status(ctx context.Context, cr connectorkit.Credentials, _ map[string]any) (any, error) {
	if strings.TrimSpace(token(cr)) == "" {
		return map[string]any{
			"paired": false,
			"detail": "Not paired. Get a code from the LYZN app: Settings → Laptop daemon → Pair a laptop.",
		}, nil
	}
	state, err := beat(ctx, cr)
	if err != nil {
		return nil, err
	}
	this := machine(cr)
	return map[string]any{
		"paired":        true,
		"machine":       this.Name,
		"daemon_id":     cr.Get(keyDaemonID),
		"capabilities":  this.Capabilities,
		"tasks_waiting": state.Tasks,
	}, nil
}

// -- argument coercion ------------------------------------------------------

// A model sends what it sends: a number as a string, an object where a list
// belongs. Coercing beats rejecting, because the failure mode of rejecting is
// the same call again in the same shape.

func strArg(in map[string]any, key string) string {
	v, ok := in[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// artifactsArg reads what the work produced, dropping anything nameless and
// placeless — a receipt row with neither is a blank line on a printout.
func artifactsArg(in map[string]any) []map[string]string {
	raw, ok := in["artifacts"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]string, 0, len(raw))
	for _, entry := range raw {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(strArg(item, "name"))
		uri := strings.TrimSpace(strArg(item, "uri"))
		if name == "" && uri == "" {
			continue
		}
		out = append(out, map[string]string{"name": name, "uri": uri})
	}
	return out
}
