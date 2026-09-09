// The work itself: the heartbeat, the queue, and what one task looks like.
package lyzn

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// beatReply is what the heartbeat answers: that we are known, and how much
// work is waiting. The count is why the poll can stop at one request on the
// overwhelming majority of ticks.
type beatReply struct {
	OK    bool `json:"ok"`
	Tasks int  `json:"tasks"`
}

// beat records that this machine is present.
//
// It also re-states the version and the capability list on every tick, which
// is deliberate: a KARMAX that was upgraded, or a laptop that has since had
// Codex installed on it, should say so without being paired again.
func beat(ctx context.Context, cr connectorkit.Credentials) (beatReply, error) {
	var out beatReply
	err := send(ctx, cr, http.MethodPost, "/daemons/heartbeat", map[string]any{
		"status":       "online",
		"version":      buildVersion(),
		"capabilities": capabilities(),
	}, &out)
	return out, err
}

// fact is one thing the conversation taught LYZN's memory.
type fact struct {
	Text string `json:"text"`
	Kind string `json:"kind"`
}

// work is one approved task, as LYZN hands it over.
//
// Every field is present in the wire form with no omitempty, because the
// thing decoding it is a program and should not have to tell "absent" from
// "empty". The context is the conversation the promise was made in, which is
// most of what makes an instruction actionable: "send it to him" is not a
// task without the sentence around it.
type work struct {
	TaskID      string `json:"taskId"`
	Text        string `json:"text"`
	Kind        string `json:"kind"`
	Quote       string `json:"quote"`
	DueAt       string `json:"dueAt"`
	RecordingID string `json:"recordingId"`
	CreatedAt   string `json:"createdAt"`
	Context     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Facts   []fact `json:"facts"`
	} `json:"context"`
}

// waiting is every approved task nobody has claimed, oldest first.
func waiting(ctx context.Context, cr connectorkit.Credentials) ([]work, error) {
	var out struct {
		Tasks []work `json:"tasks"`
	}
	if err := send(ctx, cr, http.MethodGet, "/daemons/work", nil, &out); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

// claim takes one task off the queue.
//
// A claim is a loan, not a transfer: LYZN holds it for fifteen minutes and
// then puts it back, so a laptop that dies mid-task strands nothing. The task
// has to be finished — or reported failed — inside that window, and a machine
// re-claiming what it already holds is not a conflict, which is what makes a
// restart survivable.
func claim(ctx context.Context, cr connectorkit.Credentials, taskID string) (work, error) {
	var out struct {
		Work work `json:"work"`
	}
	err := send(ctx, cr, http.MethodPost,
		"/daemons/work/"+url.PathEscape(taskID)+"/claim", nil, &out)
	return out.Work, err
}

// outcome is what a finished task reports. LYZN accepts two words, and the
// difference between them is whether the receipt says the promise was kept.
const (
	outcomeSuccess = "success"
	outcomeFailure = "failure"
)

// report closes a task and prints the receipt in one write on LYZN's side.
//
// Idempotent there, so a result posted twice — a reply lost on the way back,
// a loop that restarted — answers with the receipt the first one printed
// rather than printing a second one for the same promise.
func report(ctx context.Context, cr connectorkit.Credentials, taskID string, body map[string]any) (map[string]any, error) {
	var out map[string]any
	err := send(ctx, cr, http.MethodPost,
		"/daemons/work/"+url.PathEscape(taskID)+"/result", body, &out)
	return out, err
}

// nowISO is the format every timestamp in this API speaks.
func nowISO() string { return time.Now().UTC().Format(time.RFC3339) }
