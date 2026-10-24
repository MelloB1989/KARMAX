package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// ask is a question an agent is blocked on (fleetctl ask): the answer to it
// goes to ch, not to the agent as a new message.
type ask struct {
	ch   chan string
	done bool // answered: another answer is a follow-up
}

// DefaultAskTimeout is how long an ask waits for its answer when it is not told.
const DefaultAskTimeout = 30 * time.Minute

// AskReply is how an ask ended: with the answer, or when its time ran out.
type AskReply struct {
	Task     string `json:"task"`
	Answer   string `json:"answer,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

// Ask files a question from an agent under its task, to whoever assigned the
// task, and blocks until the answer arrives (Answer, Followup) or timeout
// passes (zero: DefaultAskTimeout). The agent goes on in the same turn. The
// question stays on the ledger after a timeout; if nobody answers it, it is
// escalated (escalate).
func (f *Fleet) Ask(ctx context.Context, from, text string, timeout time.Duration, now time.Time) (AskReply, error) {
	if _, ok := f.cfg.Worker(from); !ok {
		return AskReply{}, fmt.Errorf("%w: only an agent asks; %q is not one", ErrBadInput, from)
	}
	if strings.TrimSpace(text) == "" {
		return AskReply{}, fmt.Errorf("%w: ask needs a question", ErrBadInput)
	}
	if timeout <= 0 {
		timeout = DefaultAskTimeout
	}
	task := f.reportTask(from, now)
	if task == "" {
		return AskReply{}, fmt.Errorf("%w: %s has no task to ask about", ErrBadInput, from)
	}
	t, err := f.openTask(task)
	if err != nil {
		return AskReply{}, err
	}
	assigner := firstNonEmpty(t.Assigner, ledger.Operator)
	a := &ask{ch: make(chan string, 1)}
	f.askMu.Lock()
	if _, busy := f.asks[task]; busy {
		f.askMu.Unlock()
		return AskReply{}, fmt.Errorf("%w: %s is already asking a question on task %s", ErrConflict, from, task)
	}
	f.asks[task] = a
	f.askMu.Unlock()
	defer func() {
		f.askMu.Lock()
		delete(f.asks, task)
		f.askMu.Unlock()
	}()

	wire := text
	if !f.inbox(assigner) {
		// A session that is told, not read: it needs to know what to do.
		wire = fmt.Sprintf("Task %s: %s asks a question and is blocked until it is answered.\n\n%s\n\nAnswer with: fleetctl answer %s \"<text>\"", task, from, text, task)
	}
	if err := f.deliverMsg(ctx, from, assigner, wire, task, ledger.KindQuestion, now, true); err != nil {
		var held *Held
		if !errors.As(err, &held) { // held: the question goes out when the assigner is back
			return AskReply{}, err
		}
	}
	f.event(from, "fleet.question", "", map[string]any{"task": task, "to": assigner, "size": len(text)})

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ans := <-a.ch:
		return AskReply{Task: task, Answer: ans}, nil
	case <-timer.C:
	case <-ctx.Done():
	}
	f.askMu.Lock()
	a.done = true // nothing is blocked now: what comes is a follow-up
	f.askMu.Unlock()
	select {
	case late := <-a.ch: // an answer that came as it ended is still the answer
		return AskReply{Task: task, Answer: late}, nil
	default:
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return AskReply{}, err
	}
	return AskReply{Task: task, TimedOut: true}, nil
}

// answerBlocked gives text to the ask blocked on a task, if one is, and files
// it under the task as the answer. It does not go through deliverMsg, so it
// is redacted here.
func (f *Fleet) answerBlocked(task, from, agent, text string, now time.Time) bool {
	text = f.redactSecrets(text)
	f.askMu.Lock()
	defer f.askMu.Unlock()
	a := f.asks[task]
	if a == nil || a.done {
		return false
	}
	if err := f.db.AddRelay(ledger.Relay{From: from, To: agent, Task: task, Text: text, Kind: ledger.KindAnswer, At: now, Delivered: true}); err != nil {
		f.Log("ledger: answer on %s: %v", task, err)
	}
	a.done = true
	a.ch <- text
	f.event(agent, "fleet.answer", "", map[string]any{"task": task, "by": from, "size": len(text)})
	return true
}

// Answer answers the question an agent is blocked on under a task: the text
// is the output of its ask, and it goes on in the same turn. With nothing
// blocked it is a follow-up. It reports whether it reached a blocked ask.
func (f *Fleet) Answer(ctx context.Context, task, from, text string, now time.Time) (bool, error) {
	t, err := f.openTask(task)
	if err != nil {
		return false, err
	}
	if f.answerBlocked(task, from, t.Agent, text, now) {
		return true, nil
	}
	return false, f.Followup(ctx, task, from, text, now)
}

// escalate looks at the questions nobody has answered. One asked of the
// operator or a session that is not answering goes, after
// thresholds.question_escalate, to the orchestrator as a peer message, which
// may answer it with fleetctl answer; one still unanswered a further window
// later is one phone alert. Both happen once per question.
func (f *Fleet) escalate(ctx context.Context, now time.Time) {
	qs, err := f.db.Unanswered()
	if err != nil {
		f.Log("ledger: unanswered questions: %v", err)
		return
	}
	window := f.cfg.Thresholds.QuestionEscalate
	orch := f.cfg.Orchestrator.Name
	for _, q := range qs {
		if !f.escalates(q.Assigner) {
			continue
		}
		switch {
		case q.Escalated.IsZero() && now.Sub(q.At) >= window:
			if orch != "" {
				if err := f.deliverMsg(ctx, q.Agent, orch, f.escalation(q, now), noTask, "", now, true); err != nil {
					var held *Held
					if !errors.As(err, &held) {
						f.Log("escalate question %d of %s: %v; trying again next tick", q.ID, q.Task, err)
						continue
					}
				}
			}
			if err := f.db.MarkEscalated(q.ID, now); err != nil {
				f.Log("ledger: mark escalated: %v", err)
			}
			f.event(q.Agent, "fleet.escalate", "", map[string]string{"task": q.Task, "to": orch})
		case !q.Escalated.IsZero() && q.Alerted.IsZero() && now.Sub(q.Escalated) >= window:
			text := fmt.Sprintf("%s has waited %s for an answer on task %s (asked of %s): %s", q.Agent, Dur(now.Sub(q.At)), q.Task, q.Assigner, clip(q.Text, 200))
			if f.notify != nil {
				if err := f.notify.Notify(ctx, text); err != nil {
					f.Log("alert: %v", err)
					continue // not marked: tried again next tick
				}
			}
			if err := f.db.MarkAlerted(q.ID, now); err != nil {
				f.Log("ledger: mark alerted: %v", err)
			}
			f.event(q.Agent, "fleet.alert", "", map[string]string{"text": text})
		}
	}
}

// escalates reports whether a question asked of this assigner goes up when
// it is not answered: the operator and a session may be gone; an agent or the
// orchestrator is a peer that answers, and a person on the tailnet is not the
// operator's to answer for.
func (f *Fleet) escalates(assigner string) bool {
	if assigner == ledger.Operator {
		return true
	}
	if _, ok := f.cfg.Worker(assigner); ok || assigner == f.cfg.Orchestrator.Name {
		return false
	}
	return !strings.Contains(assigner, "@") || strings.Contains(assigner, "/")
}

// escalation is the message the orchestrator gets: the task, the question and
// the first lines of the brief, and what to do about it.
func (f *Fleet) escalation(q ledger.Question, now time.Time) string {
	brief := "(no brief on the ledger)"
	if th, err := f.db.TaskThread(q.Task); err == nil {
		for _, m := range th {
			if m.From != q.Agent {
				brief = firstLines(m.Text, 5, 600)
				break
			}
		}
	}
	return fmt.Sprintf("Task %s: %s asked %s a question %s ago and nobody has answered.\n\nQuestion: %s\n\nThe brief begins:\n%s\n\n"+
		"If you can answer it, run: fleetctl answer %s \"<text>\" — %s is blocked on it and goes on with your answer. "+
		"If you cannot, leave it: you are told nothing further, and the operator gets one alert if it stays unanswered.",
		q.Task, q.Agent, q.Assigner, Dur(now.Sub(q.At)), q.Text, brief, q.Task, q.Agent)
}

// firstLines is the first n lines of s, at most max bytes.
func firstLines(s string, n, max int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return clip(strings.Join(lines, "\n"), max)
}
