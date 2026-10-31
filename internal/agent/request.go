package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// Requests: something somebody asked KARMAX for, owned until it is done and
// its answer has reached them.
//
// A request arrives through one of several doors — a message on the
// operator's own chat, a workflow's ask() proxying a message from a chat it
// watches, the app's chat — and starts as a turn. Most finish there. One that
// cannot (the turn ran out of time having started the work, the screen judged
// it heavy, or the model itself decided it is long and handed it over with
// task.create) continues as a task: the same request, its conversation's
// context and its origin, worked in a session of its own until it is done,
// with the requester told "on it" now and given the result when it exists.
//
// The request is journalled (agent_turns) before any work starts, and the
// move to a task is one transaction (store.TakeOverRequest), so a crash at any
// point leaves it in exactly one owner's hands.

// Request kinds, by the door they came through. The journal's event_kind for
// a request with no event of its own (RequestAsk, RequestChat) is the kind.
const (
	RequestMessage = "comms.message"
	RequestAsk     = "request.ask"
	RequestChat    = "request.chat"
)

// Request is one thing asked for. A turn's call carries it in its context
// (WithRequest), and whatever the turn could not finish continues from it.
type Request struct {
	// ID names the request: the journal's event_id. A message's is its
	// event's id; ask and chat requests are given one.
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Text is what was asked, in full — the prompt the turn was given, so
	// the work can continue from it without the turn.
	Text string `json:"text"`
	// Asked is the requester's own words, when they are known apart from
	// the prompt: the message itself. It names the request in lists.
	Asked string `json:"asked,omitempty"`
	// ChannelID and Target are where the answer goes. Both empty means the
	// operator, in the app.
	ChannelID string `json:"channel_id,omitempty"`
	Target    string `json:"target,omitempty"`
	// Where says where the request came from in words, for the task and the
	// operator: "the operator's chat", "the WhatsApp group Lakeside".
	Where string `json:"where,omitempty"`
	// Loop is the workflow that proxied the request, when one did.
	Loop    string    `json:"loop,omitempty"`
	AgentID string    `json:"agent_id,omitempty"`
	Started time.Time `json:"started"`

	// taskID is set once a task has taken the request over; settled closes
	// when the turn's outcome — finished, failed or handed over — is known.
	mu      sync.Mutex
	taskID  string
	turnCtx string
	settled chan struct{}
	once    sync.Once
}

// NewRequest starts a request of kind with the given id.
func NewRequest(id, kind string) *Request {
	return &Request{ID: id, Kind: kind, Started: time.Now(), settled: make(chan struct{})}
}

// TaskID is the task that owns the request now, or "" while the turn does.
func (r *Request) TaskID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.taskID
}

func (r *Request) setTaskID(id string) {
	r.mu.Lock()
	r.taskID = id
	r.mu.Unlock()
}

// TakenBy records that a task took the request over while its turn was
// still running — the model handed it over itself with task.create.
func (r *Request) TakenBy(taskID string) { r.setTaskID(taskID) }

// TurnContext is the context block the turn serving the request was given:
// what a task continuing it needs to know.
func (r *Request) TurnContext() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turnCtx
}

func (r *Request) setTurnContext(s string) {
	r.mu.Lock()
	r.turnCtx = s
	r.mu.Unlock()
}

// Settle records that the turn's outcome — finished, failed or handed over —
// is known, for a caller that runs a request's turn itself.
func (r *Request) Settle() { r.settle() }

// settle records that the turn's outcome is known.
func (r *Request) settle() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		if r.settled != nil {
			close(r.settled)
		}
	})
}

// waitSettled waits, at most d, for the turn's outcome: the rest of a turn
// can arrive a moment before the turn's caller has decided whether a task
// takes it over, and must go where that decision sends it.
func (r *Request) waitSettled(d time.Duration) {
	if r == nil || r.settled == nil {
		return
	}
	select {
	case <-r.settled:
	case <-time.After(d):
	}
}

// JSON is the request as the journal keeps it.
func (r *Request) JSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// RequestFromJSON reads a journalled request back.
func RequestFromJSON(s string) (*Request, error) {
	r := &Request{settled: make(chan struct{})}
	if err := json.Unmarshal([]byte(s), r); err != nil {
		return nil, err
	}
	return r, nil
}

type requestKey struct{}

// WithRequest marks the call carrying ctx as working on req. A nil req marks
// it as working on none, hiding any request an outer call carried.
func WithRequest(ctx context.Context, req *Request) context.Context {
	return context.WithValue(ctx, requestKey{}, req)
}

// RequestFrom is the request the call carrying ctx works on, or nil.
func RequestFrom(ctx context.Context) *Request {
	r, _ := ctx.Value(requestKey{}).(*Request)
	return r
}

// ErrUnfinished marks a turn the harness took and could not finish: it ran
// out of time, or failed after it had begun acting. It is never retried on
// the fallback (that would do the work twice, on a weaker model with none of
// the conversation); a request it served continues as a task.
var ErrUnfinished = errors.New("the turn could not finish")

// Handoff is a request passed from its turn to a task.
type Handoff struct {
	Request *Request
	// Context is what the turn knew that the work needs: the turn's own
	// context block (who, policy, recent actions, memory) and the thread.
	Context string
	// Done is what the turn already did, so the task does not do it twice.
	Done []string
	// Why it is being handed over, in words.
	Why string
}

// HandoffResult is the task that owns the request now, and what the
// requester was told.
type HandoffResult struct {
	TaskID string
	// Ack is the acknowledgement. Already delivered to the request's channel
	// when it has one; returned for a caller that answers directly (the app).
	Ack string
}

// TaskOwner is where requests go when their turn cannot finish them. The
// runtime implements it over the task store and runner.
type TaskOwner interface {
	// HandOff makes a task the owner of the request (atomically with its
	// turn's journal row), starts it, and acknowledges the requester.
	HandOff(ctx context.Context, h Handoff) (HandoffResult, error)
	// LateResult gives the task the rest of the turn it took over from, when
	// the session finished that turn after the hand-off.
	LateResult(taskID string, t HarnessTurn)
	// InFlight describes requests tasks are working on for this chat (""
	// for the app), so a turn about the same chat does not answer them again.
	InFlight(target string) []string
}

// SetTaskOwner installs where unfinished requests go. Nil leaves a request's
// unfinished turn to fail and be reported as before.
func (a *Agent) SetTaskOwner(o TaskOwner) {
	a.mu.Lock()
	a.taskOwner = o
	a.mu.Unlock()
}

func (a *Agent) owner() TaskOwner {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.taskOwner
}

// CurrentRequest is the request this agent's thinking is working on right
// now, if any. Its session runs one turn at a time, so a tool the turn calls
// through the shell (task.create, claude_code.call) belongs to it.
func (a *Agent) CurrentRequest() *Request {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.current
}

// delegationOrigin is the request a delegation started now serves: the one
// the agent's thinking is on.
func (a *Agent) delegationOrigin() builtin.DelegationOrigin {
	req := a.CurrentRequest()
	if req == nil {
		return builtin.DelegationOrigin{}
	}
	return builtin.DelegationOrigin{RequestID: req.ID, ChannelID: req.ChannelID, Target: req.Target, Where: req.Where}
}

// working marks req as the turn in progress until the returned func is called.
func (a *Agent) working(req *Request) func() {
	if req == nil {
		return func() {}
	}
	a.mu.Lock()
	prev := a.current
	a.current = req
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		if a.current == req {
			a.current = prev
		}
		a.mu.Unlock()
	}
}

// journalRequest writes the request's journal row before any work starts —
// for a request with no event of its own; a message's row is its event's,
// opened by the router. A store that cannot write it does not stop the
// request: losing crash-safety is better than losing the request.
func (a *Agent) journalRequest(req *Request) {
	if a.store == nil || req == nil || req.Kind == RequestMessage {
		return
	}
	if req.AgentID == "" {
		req.AgentID = a.def.ID
	}
	if req.Started.IsZero() {
		req.Started = time.Now()
	}
	if _, err := a.store.StartAgentTurn(store.AgentTurn{
		ID: "turn-" + req.ID, AgentID: a.def.ID, EventID: req.ID, EventKind: req.Kind,
		EventJSON: req.JSON(), Attempt: 1, StartedAt: req.Started,
	}); err != nil {
		a.log.Warn("could not journal a request; working it anyway", zap.String("request", req.ID), zap.Error(err))
	}
}

// finishRequest closes a journalled request's row. A row a task took over
// keeps saying so (store.FinishAgentTurn).
func (a *Agent) finishRequest(req *Request, err error) {
	if a.store == nil || req == nil || req.Kind == RequestMessage {
		return
	}
	status, msg := store.TurnOK, ""
	if err != nil {
		status, msg = store.TurnFailed, err.Error()
	}
	if ferr := a.store.FinishAgentTurn(req.ID, status, msg); ferr != nil {
		a.log.Warn("could not close a request's journal row", zap.String("request", req.ID), zap.Error(ferr))
	}
}

// unfinished reports whether a turn's failure means its work should go on
// elsewhere rather than be reported as a failure.
func unfinished(err error) bool { return errors.Is(err, ErrUnfinished) }

// handOff passes a request whose turn cannot finish it to the task owner.
// ok is false when there is no owner or it refused; the turn then fails as
// it would have.
func (a *Agent) handOff(ctx context.Context, req *Request, turnCtx string, calls []karmahelper.ToolCallRecord, cause error, why string) (HandoffResult, bool) {
	o := a.owner()
	if o == nil || req == nil {
		return HandoffResult{}, false
	}
	if cause == nil {
		cause = errors.New(why)
	}
	done := a.doneDuring(req.Started, &turnFailure{calls: calls, err: cause})
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	res, err := o.HandOff(ctx, Handoff{Request: req, Context: turnCtx, Done: done, Why: why})
	if err != nil {
		a.log.Error("could not hand a request over to a task", zap.String("request", req.ID), zap.Error(err))
		return HandoffResult{}, false
	}
	req.setTaskID(res.TaskID)
	a.log.Info("a request continues as a task",
		zap.String("request", req.ID), zap.String("task", res.TaskID), zap.String("why", why))
	return res, true
}

// requestLate is the late-reply handler for a request's turn: the rest of a
// turn a task took over belongs to that task; otherwise it goes where the
// answer would have (fallback).
func (a *Agent) requestLate(req *Request, fallback func(HarnessTurn)) func(HarnessTurn) {
	return LateForRequest(req, a.owner, fallback)
}

// LateForRequest is the late-reply handler for any caller running a
// request's turn: it waits for the turn's outcome, then gives the rest of the
// turn to the task that took the request over, or to fallback.
func LateForRequest(req *Request, owner func() TaskOwner, fallback func(HarnessTurn)) func(HarnessTurn) {
	return func(t HarnessTurn) {
		req.waitSettled(time.Minute)
		if id := req.TaskID(); id != "" {
			if o := owner(); o != nil {
				o.LateResult(id, t)
				return
			}
		}
		if fallback != nil {
			fallback(t)
		}
	}
}

// requestContext tells the turn which request it is serving and how to hand
// it over when it is long — so the model's own judgement is a way in, not
// only the clock — and which requests from the same place are already owned.
func (a *Agent) requestContext(req *Request) string {
	if req == nil || a.owner() == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## This request\n\nRequest id: %s (from %s).\n", req.ID, orDefault(req.Where, "the operator"))
	b.WriteString("If doing it properly takes more than a couple of minutes — a document, research, code, " +
		"many steps, waiting on something — do not try to fit it into this turn. Hand it over now:\n\n" +
		"    karmax tool call task.create request=" + req.ID + " goal=\"<everything the work needs: what, for whom, constraints, what done looks like>\"\n\n" +
		"The task works it in its own session, survives restarts, and delivers the result back to where this came from. " +
		"Then answer with one short line saying you are on it — and do not claim any of it is done.\n")
	if inflight := a.owner().InFlight(req.Target); len(inflight) > 0 {
		b.WriteString("\nAlready being worked on as tasks for this chat (each will deliver its own result here — " +
			"do not answer them again, ask for what they need, or start them twice):\n")
		for _, l := range inflight {
			b.WriteString("- " + l + "\n")
		}
	}
	return b.String()
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
