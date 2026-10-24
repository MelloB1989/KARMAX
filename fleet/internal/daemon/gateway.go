package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/outbound"
)

// The KARMAX gateway: agents use the operator's integrations that KARMAX
// holds (mail, calendar, WhatsApp, GitHub, …) without ever holding KARMAX's
// token. A read runs at once; a write is filed as an approval request, the
// orchestrator is told, and it runs only once she approves it; a denied tool
// never runs. Who may use it at all is the API's to decide (server).

// Gateway is KARMAX's tool API as fleetd calls it; outbound.Karmax is the
// real one.
type Gateway interface {
	Tools(ctx context.Context) ([]outbound.KarmaxTool, error)
	Call(ctx context.Context, name string, args map[string]any) (outbound.KarmaxResult, error)
}

// ErrRefused is a call the gateway's policy or its rules refuse.
var ErrRefused = errors.New("refused")

// Requester is who calls the gateway: an agent by its verified name, on its
// current task, or a full-scope caller by the name it gives.
type Requester struct {
	Name, Task string
}

// GatewayTool is a KARMAX tool an agent may call, with its class.
type GatewayTool struct {
	outbound.KarmaxTool
	Class string `json:"class"`
}

// CallReply is what a gateway call came to: the tool's result once it ran,
// or the approval request a write is waiting on, or was refused by.
type CallReply struct {
	Tool     string                 `json:"tool"`
	Class    string                 `json:"class"`
	Result   *outbound.KarmaxResult `json:"result,omitempty"`
	Approval *ledger.Approval       `json:"approval,omitempty"`
	// Notice is something the caller should know: the orchestrator could
	// not be told of the request.
	Notice string `json:"notice,omitempty"`
}

// ApprovalTimeout is how long a write waits for a decision unless the caller
// says; MaxApprovalTimeout is the longest it may.
const (
	ApprovalTimeout    = 30 * time.Minute
	MaxApprovalTimeout = 24 * time.Hour
)

func (f *Fleet) gateway() (Gateway, error) {
	if f.Karmax == nil {
		return nil, fmt.Errorf("this fleetd has no KARMAX gateway (KARMAX_API_TOKEN is not set)")
	}
	return f.Karmax, nil
}

// KarmaxTools is every KARMAX tool the gateway lets through, with its class;
// denied tools are left out.
func (f *Fleet) KarmaxTools(ctx context.Context) ([]GatewayTool, error) {
	g, err := f.gateway()
	if err != nil {
		return nil, err
	}
	tools, err := g.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := []GatewayTool{}
	for _, t := range tools {
		if c := f.cfg.Karmax.ToolClass(t.Name); c != config.ToolDeny {
			out = append(out, GatewayTool{KarmaxTool: t, Class: c})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// KarmaxCall calls a tool for who. A read runs now. A write is filed as a
// pending request the orchestrator is told of, which waits at most timeout
// for her decision (AwaitApproval). A denied tool is refused, with nothing
// filed.
func (f *Fleet) KarmaxCall(ctx context.Context, who Requester, tool string, args map[string]any, reason string, timeout time.Duration, now time.Time) (CallReply, error) {
	g, err := f.gateway()
	if err != nil {
		return CallReply{}, err
	}
	if who.Name == "" {
		return CallReply{}, fmt.Errorf("%w: a gateway call needs a caller", ErrBadInput)
	}
	if args == nil {
		args = map[string]any{}
	}
	class := f.cfg.Karmax.ToolClass(tool)
	reply := CallReply{Tool: tool, Class: class}
	audit := func(decision string, more map[string]any) {
		d := map[string]any{"tool": tool, "task": who.Task, "class": class, "decision": decision, "args": argNames(args)}
		for k, v := range more {
			d[k] = v
		}
		f.event(who.Name, "karmax.call", "", d)
	}
	switch class {
	case config.ToolDeny:
		audit("refused", nil)
		return reply, fmt.Errorf("%w: %s is not open to agents through fleetd", ErrRefused, tool)
	case config.ToolRead:
		res, err := g.Call(ctx, tool, args)
		audit("ran", map[string]any{"ok": err == nil && res.OK})
		if err != nil {
			return reply, err
		}
		reply.Result = &res
		return reply, nil
	}
	if strings.TrimSpace(reason) == "" {
		return reply, fmt.Errorf("%w: a write needs --reason: the orchestrator decides on it", ErrBadInput)
	}
	if timeout <= 0 {
		timeout = ApprovalTimeout
	}
	timeout = min(timeout, MaxApprovalTimeout)
	b, err := json.Marshal(args)
	if err != nil {
		return reply, fmt.Errorf("%w: arguments: %v", ErrBadInput, err)
	}
	a := ledger.Approval{Agent: who.Name, Task: who.Task, Tool: tool, Args: b, Reason: reason, State: ledger.Pending,
		At: now, Expires: now.Add(timeout)}
	if a.ID, err = f.db.AddApproval(a); err != nil {
		return reply, err
	}
	reply.Approval = &a
	audit("requested", map[string]any{"approval": a.ID})
	if err := f.askOrchestrator(ctx, a, now); err != nil {
		var held *Held
		if !errors.As(err, &held) {
			f.Log("gateway: approval %d: the orchestrator was not told: %v", a.ID, err)
			reply.Notice = fmt.Sprintf("the orchestrator could not be told (%v); ask her to look at fleetctl approvals", err)
		}
	}
	return reply, nil
}

// askOrchestrator tells the orchestrator of a request: the tool, its
// arguments, the reason, the task's brief, and how to answer.
func (f *Fleet) askOrchestrator(ctx context.Context, a ledger.Approval, now time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "KARMAX gateway: approval request %d — %s asks to call %s.\n", a.ID, a.Agent, a.Tool)
	if a.Task != "" {
		fmt.Fprintf(&b, "Task %s: %s\n", a.Task, f.firstBriefLine(a.Task))
	}
	args := string(a.Args)
	if len(args) > 2000 {
		args = args[:2000] + "…"
	}
	fmt.Fprintf(&b, "Arguments: %s\nReason: %s\n", args, a.Reason)
	fmt.Fprintf(&b, "It waits until %s. Approve: fleetctl approve %d — or deny: fleetctl deny %d --reason \"<why>\".\n",
		a.Expires.UTC().Format("15:04 MST"), a.ID, a.ID)
	b.WriteString("If it sends, deletes or changes anything on the operator's behalf, ask the operator on WhatsApp first and decide from their answer.")
	// From the agent, so it reads as its request; a caller that is no
	// agent (a full-scope session) is the operator's.
	from := a.Agent
	if f.known(from) != nil {
		from = ledger.Operator
	}
	return f.send(ctx, from, f.cfg.Orchestrator.Name, b.String(), a.Task, now)
}

// firstBriefLine is the first line of the first message the task's agent was
// given: its brief.
func (f *Fleet) firstBriefLine(task string) string {
	t, err := f.db.Task(task)
	if err != nil {
		return "(no such task in the ledger)"
	}
	thread, _ := f.db.TaskThread(task)
	for _, r := range thread {
		if r.From != t.Agent && strings.TrimSpace(r.Text) != "" {
			line, _, _ := strings.Cut(strings.TrimSpace(r.Text), "\n")
			if len(line) > 300 {
				line = line[:300] + "…"
			}
			return fmt.Sprintf("%q (from %s)", line, t.Assigner)
		}
	}
	return fmt.Sprintf("(no brief on the ledger; assigned by %s)", t.Assigner)
}

// AwaitApproval waits for the decision on who's own request, until ctx ends
// or the request expires. An approved request is run, once, and its result
// returned; a denied or expired one comes back as refused. A request still
// pending when ctx ends comes back as it is, for the caller to wait again.
func (f *Fleet) AwaitApproval(ctx context.Context, who Requester, id int64) (CallReply, error) {
	g, err := f.gateway()
	if err != nil {
		return CallReply{}, err
	}
	a, err := f.approval(id)
	if err != nil {
		return CallReply{}, err
	}
	// Only the agent that asked runs it, and only on the task it asked on.
	if a.Agent != who.Name || a.Task != who.Task {
		return CallReply{}, fmt.Errorf("%w: approval %d is not a request of yours on this task", ErrRefused, id)
	}
	every := f.waitEvery
	for {
		if a.State == ledger.Pending && !time.Now().Before(a.Expires) {
			f.expire(time.Now())
			if a, err = f.approval(id); err != nil {
				return CallReply{}, err
			}
		}
		reply := CallReply{Tool: a.Tool, Class: config.ToolWrite, Approval: &a}
		switch a.State {
		case ledger.Denied, ledger.Expired, ledger.Ran:
			return reply, refusal(a)
		case ledger.Approved:
			if time.Now().After(a.Expires) { // approved, but nobody came for it in time
				return reply, fmt.Errorf("%w: approval %d was approved after its wait ended; ask again", ErrRefused, id)
			}
			won, err := f.db.MoveApproval(id, ledger.Approved, ledger.Ran, "", "", time.Now())
			if err != nil {
				return reply, err
			}
			if !won {
				break // another waiter took it; look again
			}
			var args map[string]any
			_ = json.Unmarshal(a.Args, &args)
			// It runs to the end even if the caller stops waiting now: it is
			// marked ran, and runs only once.
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Minute)
			res, cerr := g.Call(cctx, a.Tool, args)
			cancel()
			note := "ok"
			switch {
			case cerr != nil:
				note = "failed: " + cerr.Error()
			case !res.OK:
				note = "tool error: " + res.Error
			}
			_, _ = f.db.MoveApproval(id, ledger.Ran, ledger.Ran, "", note, time.Now())
			f.event(a.Agent, "karmax.call", "", map[string]any{"tool": a.Tool, "task": a.Task, "class": config.ToolWrite,
				"decision": "approved", "approval": id, "by": a.DecidedBy, "args": argNames(args), "ok": cerr == nil && res.OK})
			a.State, a.Note = ledger.Ran, note
			if cerr != nil {
				return reply, cerr
			}
			reply.Result = &res
			return reply, nil
		}
		wait := min(every, time.Until(a.Expires))
		select {
		case <-ctx.Done():
			return reply, nil
		case <-time.After(max(wait, time.Millisecond)):
		}
		if a, err = f.approval(id); err != nil {
			return CallReply{}, err
		}
	}
}

func refusal(a ledger.Approval) error {
	switch a.State {
	case ledger.Denied:
		why := ""
		if a.Note != "" {
			why = ": " + a.Note
		}
		return fmt.Errorf("%w: %s denied approval %d%s", ErrRefused, a.DecidedBy, a.ID, why)
	case ledger.Expired:
		return fmt.Errorf("%w: approval %d expired with no decision", ErrRefused, a.ID)
	}
	return fmt.Errorf("%w: approval %d has already run", ErrRefused, a.ID)
}

func (f *Fleet) approval(id int64) (ledger.Approval, error) {
	a, err := f.db.GetApproval(id)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("%w: no approval %d", ErrNoTask, id)
	}
	return a, err
}

// expire moves every request past its time to expired, each one an event.
func (f *Fleet) expire(now time.Time) {
	gone, err := f.db.ExpireApprovals(now)
	if err != nil {
		f.Log("ledger: expire approvals: %v", err)
	}
	for _, a := range gone {
		f.event(a.Agent, "karmax.decision", "", map[string]any{"tool": a.Tool, "task": a.Task, "class": config.ToolWrite,
			"decision": ledger.Expired, "approval": a.ID})
	}
}

// Approvals lists requests in a state ("" for all, newest limit of them).
func (f *Fleet) Approvals(state string, limit int) ([]ledger.Approval, error) {
	f.expire(time.Now())
	return f.db.Approvals(state, limit)
}

// Decide approves or denies a pending request. Nobody decides on their own
// request; one that has expired or been decided stays as it is.
func (f *Fleet) Decide(id int64, by string, approve bool, reason string, now time.Time) (ledger.Approval, error) {
	f.expire(now)
	a, err := f.approval(id)
	if err != nil {
		return a, err
	}
	if by == "" {
		return a, fmt.Errorf("%w: a decision needs who made it", ErrBadInput)
	}
	if by == a.Agent {
		return a, fmt.Errorf("%w: %s asked for approval %d and cannot decide on it", ErrRefused, by, id)
	}
	to := ledger.Denied
	if approve {
		to, reason = ledger.Approved, ""
	}
	won, err := f.db.MoveApproval(id, ledger.Pending, to, by, reason, now)
	if err != nil {
		return a, err
	}
	if !won {
		cur, _ := f.approval(id)
		return cur, fmt.Errorf("%w: approval %d is %s, not pending", ErrConflict, id, cur.State)
	}
	f.event(a.Agent, "karmax.decision", "", map[string]any{"tool": a.Tool, "task": a.Task, "class": config.ToolWrite,
		"decision": to, "approval": id, "by": by})
	return f.approval(id)
}

// argNames is the names of a call's arguments, never their values: the
// audit trail says what was sent where, not what it said.
func argNames(args map[string]any) []string {
	out := make([]string, 0, len(args))
	for k := range args {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
