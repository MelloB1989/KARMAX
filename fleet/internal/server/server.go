// Package server is fleetd's HTTP API, which fleetctl talks to.
//
// Three token scopes: Full (you, and the orchestrator) can do everything;
// Relay (every agent container) can read status, tell, ask its assigner a
// question and wait for the answer, report to the operator, and read a
// secret it is granted; Console (fleet-console) reads everything and prompts and rotates
// as the operator. Requests with none are refused, on every listener;
// GET /v1 lists every route and the scopes it allows.
//
// On the tailnet listener (Tailnet) there is no token: the caller is who
// Tailscale says, an operator (Full's scope) or a member, who runs tasks as
// themselves and sees and closes only their own.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/archive"
	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/guide"
	"github.com/MelloB1989/karmax/fleet/internal/identity"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/memory"
	"github.com/MelloB1989/karmax/fleet/internal/outbound"
)

// Ops is what the API drives; *daemon.Fleet is the real one.
type Ops interface {
	Status(now time.Time) []daemon.Row
	// IsWorker reports whether a name is a worker of fleet.yaml: an agent,
	// or one of its slots that exists (agent-03:2 with slots: 2).
	IsWorker(name string) bool
	TellKind(ctx context.Context, from, to, text, kind string, now time.Time) error
	Ask(ctx context.Context, from, text string, timeout time.Duration, now time.Time) (daemon.AskReply, error)
	Answer(ctx context.Context, task, from, text string, now time.Time) (bool, error)
	Assign(ctx context.Context, name, task, from, brief string, now time.Time) error
	Followup(ctx context.Context, task, from, text string, now time.Time) error
	Cancel(ctx context.Context, task, from, reason string, now time.Time) error
	Pick(repo string, now time.Time) daemon.PickReply
	Run(ctx context.Context, in daemon.RunRequest, now time.Time) (daemon.RunReply, error)
	Done(name, task, outcome, branch, pr string, now time.Time) error
	Close(name, task, outcome, branch, pr string, now time.Time) error
	Rotate(name string, dryRun bool, now time.Time) (daemon.RotatePlan, error)
	Enable(name string) error
	Model(name string) (daemon.ModelReply, error)
	SetModel(name, model string, now time.Time) error
	Restore(ctx context.Context, id, onto string, now time.Time) error
	InboxOf(to string, after int64) ([]ledger.Relay, error)
	Wait(ctx context.Context, agent, task string, after int64) (daemon.WaitResult, error)
	WaitNew(ctx context.Context, agent, task, reader string) (daemon.WaitResult, error)
	Result(task string) (daemon.TaskResult, error)

	Events(q daemon.EventsQuery) ([]ledger.Event, error)
	Usage(by string, since time.Time) ([]ledger.UsageRow, error)
	History(agent string, n int) ([]ledger.Session, error)
	Quota() (map[string]ledger.Quota, error)
	Tasks(open bool, agent, assigner string, n int) ([]ledger.Task, error)
	TaskAgent(task string) (*ledger.Task, error)
	Activity(since time.Time, bucket time.Duration, now time.Time) (daemon.ActivityReply, error)
	Today(now time.Time) (daemon.TodayReply, error)
	Peek(ctx context.Context, agent string, n int) (string, error)
	Prompt(ctx context.Context, agent, text string) error
	Archives(since time.Time) ([]*archive.Manifest, error)
	Archive(id string) (*daemon.ArchiveDetail, error)
	PruneArchives(before time.Time) (int, error)
	Render() (string, error)

	SecretPut(name string, value []byte, now time.Time) error
	Secrets(now time.Time) ([]ledger.SecretInfo, error)
	SecretGrant(name, task, agent, by string, ttl time.Duration, now time.Time) error
	SecretRevoke(name, task, agent string) error
	SecretRemove(name string) error
	SecretRead(agent, name string, now time.Time) ([]byte, error)
	Up(ctx context.Context, hosts []string) (string, error)
	Down(ctx context.Context, hosts []string) (string, error)

	Recall(ctx context.Context, query string, limit int) (*memory.Result, error)
	Suggest(ctx context.Context, from, text, why string, now time.Time) (string, error)
	OperatorAssigner(assigner string) bool
	KarmaxTools(ctx context.Context) ([]daemon.GatewayTool, error)
	KarmaxCall(ctx context.Context, who daemon.Requester, tool string, args map[string]any, reason string, timeout time.Duration, now time.Time) (daemon.CallReply, error)
	AwaitApproval(ctx context.Context, who daemon.Requester, id int64) (daemon.CallReply, error)
	Approvals(state string, limit int) ([]ledger.Approval, error)
	Decide(id int64, by string, approve bool, reason string, now time.Time) (ledger.Approval, error)
}

// Tokens are the bearer tokens, one per scope.
type Tokens struct{ Full, Relay, Console string }

// A scope is what a token may do. Each route names the scopes it allows.
type scope string

const (
	none    scope = ""
	full    scope = "full"    // the operator and the orchestrator
	relay   scope = "relay"   // every agent: status, tell, its reports
	console scope = "console" // fleet-console: reads, prompt, rotate, as the operator
	member  scope = "member"  // a person on the tailnet: their own tasks
)

var (
	everyone = []scope{full, relay, console}
	readers  = []scope{full, console}
	fullOnly = []scope{full}
)

// and is a route's scopes and member: what a person on the tailnet may do
// too, on their own tasks.
func and(s []scope, more scope) []scope { return append(slices.Clone(s), more) }

func (t Tokens) scope(r *http.Request) scope {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	eq := func(want string) bool {
		return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
	}
	switch {
	case eq(t.Full):
		return full
	case eq(t.Relay):
		return relay
	case eq(t.Console):
		return console
	}
	return none
}

// Version is fleetd's build, set with -ldflags "-X …/server.Version=…".
var Version = "dev"

// Route is one entry of the API's index (GET /v1).
type Route struct {
	Route  string   `json:"route"`
	Scopes []string `json:"scopes"`
}

// Handler is the API. orch, when set, reports the orchestrator's own quota.
func Handler(ops Ops, tok Tokens, orch func() *outbound.Quota) http.Handler {
	h, _ := DrainingHandler(ops, tok, orch)
	return h
}

// errDraining is fleetd shutting down: a long poll it ends is answered 503,
// and fleetctl waits for fleetd to be back and asks again.
var errDraining = errors.New("fleetd is shutting down; try again in a moment")

// DrainingHandler is Handler with a drain: once it is called, every long
// poll in flight (wait, ask, the wait on an approval) ends with a 503 at
// once, and ones that come later get it too. fleetd calls it before it closes
// the ledger, so no poll is answered from a ledger that is gone.
func DrainingHandler(ops Ops, tok Tokens, orch func() *outbound.Quota) (http.Handler, func()) {
	drain := make(chan struct{})
	var once sync.Once
	return handler(ops, tok, orch, drain), func() { once.Do(func() { close(drain) }) }
}

// unanswered is a long poll's result that says nothing: it ran out of time
// or was cut short. One that carries an answer is delivered even as fleetd
// drains, since handing it over may have moved a cursor.
func unanswered(v any, err error) bool {
	switch v := v.(type) {
	case daemon.WaitResult:
		return err != nil || v.Reason == "timeout"
	case daemon.AskReply:
		return err != nil || v.TimedOut
	case daemon.CallReply:
		return err != nil || v.Result == nil
	}
	return err != nil
}

func handler(ops Ops, tok Tokens, orch func() *outbound.Quota, drain <-chan struct{}) http.Handler {
	mux := http.NewServeMux()
	// long is a handler that holds its request open: fleetd draining cuts it
	// short with errDraining unless it already has its answer.
	long := func(h func(w http.ResponseWriter, r *http.Request) (any, error)) func(w http.ResponseWriter, r *http.Request) (any, error) {
		return func(w http.ResponseWriter, r *http.Request) (any, error) {
			ctx, cancel := context.WithCancelCause(r.Context())
			defer cancel(nil)
			go func() {
				select {
				case <-drain:
					cancel(errDraining)
				case <-ctx.Done():
				}
			}()
			v, err := h(w, r.WithContext(ctx))
			if errors.Is(context.Cause(ctx), errDraining) && unanswered(v, err) {
				return nil, errDraining
			}
			return v, err
		}
	}
	started := time.Now()
	var index []Route
	may := map[scope][]string{}
	route := func(pattern string, h func(w http.ResponseWriter, r *http.Request) (any, error), allowed ...scope) {
		names := make([]string, len(allowed))
		for i, a := range allowed {
			names[i] = string(a)
			may[a] = append(may[a], pattern)
		}
		index = append(index, Route{Route: pattern, Scopes: names})
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			s := tok.scope(r)
			if c := callerOf(r); c != nil {
				s = c.scope() // the tailnet: who, not a token
			}
			if s == none {
				writeErr(w, http.StatusUnauthorized, fmt.Errorf("a fleet token is required (FLEET_TOKEN)"))
				return
			}
			if !slices.Contains(allowed, s) {
				who := fmt.Sprintf("this token (scope %s)", s)
				if c := callerOf(r); c != nil {
					who = fmt.Sprintf("%s (%s)", c.Login, c.Role)
				}
				writeErr(w, http.StatusForbidden, fmt.Errorf("%s may: %s", who, strings.Join(may[s], ", ")))
				return
			}
			v, err := h(w, r.WithContext(context.WithValue(r.Context(), scopeKey{}, s)))
			var held *daemon.Held
			if errors.As(err, &held) { // taken, to be delivered later: not a failure
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "queued": true, "why": held.Error()})
				return
			}
			if err != nil {
				code := http.StatusInternalServerError
				if be, ok := err.(badRequest); ok {
					code, err = http.StatusBadRequest, be.error
				}
				if errors.Is(err, daemon.ErrNoTask) || errors.Is(err, ledger.ErrNoSecret) || errors.Is(err, daemon.ErrNoGrant) {
					err = notFound{err}
				}
				if errors.Is(err, daemon.ErrForbidden) {
					err = forbidden{err}
				}
				if errors.Is(err, daemon.ErrBadInput) {
					code = http.StatusBadRequest
				}
				if errors.Is(err, daemon.ErrRefused) {
					code = http.StatusForbidden
				}
				if errors.Is(err, daemon.ErrMemoryOff) {
					code = http.StatusNotImplemented
				}
				if errors.Is(err, errDraining) {
					code = http.StatusServiceUnavailable
				}
				if errors.Is(err, daemon.ErrConflict) {
					code = http.StatusConflict
					var nf *daemon.NoFreeAgent
					if errors.As(err, &nf) { // say why each agent was passed over
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(code)
						_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "candidates": nf.Candidates})
						return
					}
				}
				if ne, ok := err.(notFound); ok {
					code, err = http.StatusNotFound, ne.error
				}
				if fe, ok := err.(forbidden); ok {
					code, err = http.StatusForbidden, fe.error
				}
				writeErr(w, code, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		})
	}
	ok := map[string]bool{"ok": true}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": Version, "uptime_s": int64(time.Since(started).Seconds())})
	})
	route("GET /v1", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return index, nil
	}, and(everyone, member)...)
	route("GET /v1/whoami", func(w http.ResponseWriter, r *http.Request) (any, error) {
		s := scopeOf(r)
		name := ledger.Operator
		if s == relay {
			name = "agent"
		}
		if c := callerOf(r); c != nil {
			name = c.Login
		}
		return map[string]string{"scope": string(s), "name": name}, nil
	}, and(everyone, member)...)
	route("GET /v1/status", func(w http.ResponseWriter, r *http.Request) (any, error) {
		out := map[string]any{"agents": ops.Status(time.Now())}
		if orch != nil {
			if q := orch(); q != nil {
				out["orchestrator"] = q
			}
		}
		return out, nil
	}, and(everyone, member)...)
	route("POST /v1/tell", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ From, To, Text, Kind string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if !ledger.ValidKind(in.Kind) {
			return nil, badRequest{fmt.Errorf("kind %q: want report, question or progress", in.Kind)}
		}
		switch scopeOf(r) {
		case console:
			in.From = ledger.Operator // the console is the operator's keyboard
		case relay:
			if ledger.IsPerson(in.From) {
				return nil, forbidden{fmt.Errorf("an agent does not speak as %s: only that person does", in.From)}
			}
			// An agent that proved who it is speaks as itself only.
			if name, ok := verifiedAgent(r); ok {
				if in.From != "" && in.From != name {
					return nil, forbidden{fmt.Errorf("this is %s's key: it does not speak as %s", name, in.From)}
				}
				in.From = name
			}
		}
		if in.From == "" || in.To == "" || strings.TrimSpace(in.Text) == "" {
			return nil, badRequest{fmt.Errorf("tell needs from, to and text")}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		return ok, ops.TellKind(ctx, in.From, in.To, in.Text, in.Kind, time.Now())
	}, everyone...)
	route("POST /v1/ask", long(func(w http.ResponseWriter, r *http.Request) (any, error) {
		var err error
		var in struct{ From, Text, Timeout string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if in.From, err = provenFrom(r, in.From); err != nil {
			return nil, err
		}
		if in.From == "" || strings.TrimSpace(in.Text) == "" {
			return nil, badRequest{fmt.Errorf("ask needs from and text")}
		}
		timeout := daemon.DefaultAskTimeout
		if in.Timeout != "" {
			if timeout, err = time.ParseDuration(in.Timeout); err != nil || timeout <= 0 {
				return nil, badRequest{fmt.Errorf("timeout %q: want a duration like 30m", in.Timeout)}
			}
		}
		// It holds the request open for the answer, and may wait for nothing
		// else: its question is the agent's own, under the agent's own task.
		ctx, cancel := context.WithTimeout(r.Context(), timeout+time.Minute)
		defer cancel()
		return ops.Ask(ctx, in.From, in.Text, timeout, time.Now())
	}), relay)
	route("POST /v1/assign", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ Agent, Task, From, Brief string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if in.Agent == "" || in.Task == "" {
			return nil, badRequest{fmt.Errorf("assign needs agent and task")}
		}
		if in.From == "" {
			in.From = ledger.Operator
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		return ok, ops.Assign(ctx, in.Agent, in.Task, in.From, in.Brief, time.Now())
	}, fullOnly...)
	route("POST /v1/run", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Brief, Repo, Agent, From, Task, Queue, Model string
		}
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Brief) == "" {
			return nil, badRequest{fmt.Errorf("run needs a brief")}
		}
		if in.Model != "" && scopeOf(r) == member {
			return nil, forbidden{fmt.Errorf("a member may not choose the model")}
		}
		var queue time.Duration
		if in.Queue != "" {
			var err error
			if queue, err = time.ParseDuration(in.Queue); err != nil || queue < 0 {
				return nil, badRequest{fmt.Errorf("queue %q: want a duration like 30m", in.Queue)}
			}
		}
		limit := 0
		if c := callerOf(r); c != nil && scopeOf(r) == member {
			in.From, limit = memberFrom(c, in.From), c.MaxOpen // a member runs as themselves (or a session of theirs), and only so many
		}
		// A queued run holds the request open for the queue time.
		ctx, cancel := context.WithTimeout(r.Context(), queue+3*time.Minute)
		defer cancel()
		return ops.Run(ctx, daemon.RunRequest{Brief: in.Brief, Repo: in.Repo, Agent: in.Agent, From: in.From,
			Task: in.Task, Model: in.Model, Queue: queue, Limit: limit}, time.Now())
	}, and(fullOnly, member)...)
	route("GET /v1/pick", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return ops.Pick(r.URL.Query().Get("repo"), time.Now()), nil
	}, and(readers, member)...)
	route("POST /v1/done", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Agent, Task, Outcome, Branch, PR string
			KeepSession                      bool `json:"keep_session"`
		}
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if in.Agent == "" && in.Task == "" {
			return nil, badRequest{fmt.Errorf("done needs agent or task")}
		}
		if err := own(r, ops, in.Task); err != nil {
			return nil, err
		}
		if scopeOf(r) == member {
			in.Agent = "" // the task's own agent, not one a member names
		}
		if in.KeepSession {
			return ok, ops.Close(in.Agent, in.Task, in.Outcome, in.Branch, in.PR, time.Now())
		}
		return ok, ops.Done(in.Agent, in.Task, in.Outcome, in.Branch, in.PR, time.Now())
	}, and(fullOnly, member)...)
	route("POST /v1/tasks/{id}/followup", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ From, Text string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, badRequest{fmt.Errorf("followup needs text")}
		}
		if err := own(r, ops, r.PathValue("id")); err != nil {
			return nil, err
		}
		if in.From == "" {
			in.From = ledger.Operator
		}
		if c := callerOf(r); c != nil && scopeOf(r) == member {
			in.From = memberFrom(c, in.From)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		return ok, ops.Followup(ctx, r.PathValue("id"), in.From, in.Text, time.Now())
	}, and(fullOnly, member)...)
	route("POST /v1/tasks/{id}/cancel", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ From, Reason string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if err := own(r, ops, r.PathValue("id")); err != nil {
			return nil, err
		}
		if in.From == "" {
			in.From = ledger.Operator
		}
		if c := callerOf(r); c != nil && scopeOf(r) == member {
			in.From = memberFrom(c, in.From)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		return ok, ops.Cancel(ctx, r.PathValue("id"), in.From, in.Reason, time.Now())
	}, and(fullOnly, member)...)
	route("POST /v1/tasks/{id}/answer", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ From, Text string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, badRequest{fmt.Errorf("answer needs text")}
		}
		id := r.PathValue("id")
		if err := own(r, ops, id); err != nil {
			return nil, err
		}
		if in.From == "" && scopeOf(r) != relay {
			in.From = ledger.Operator
		}
		switch c := callerOf(r); {
		case c != nil && scopeOf(r) == member:
			in.From = memberFrom(c, in.From)
		case scopeOf(r) == relay:
			// An agent answers only what it assigned: a peer's question, not a
			// task someone else gave.
			t, err := ops.TaskAgent(id)
			if err != nil {
				return nil, err
			}
			from, err := provenFrom(r, in.From)
			if err != nil {
				return nil, err
			}
			in.From = from
			if ledger.IsPerson(in.From) || t.Assigner != in.From {
				return nil, forbidden{fmt.Errorf("task %s was not assigned by %q: an agent answers only its own tasks' questions", id, in.From)}
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		blocked, err := ops.Answer(ctx, id, in.From, in.Text, time.Now())
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "answered": blocked}, nil
	}, and(and(fullOnly, relay), member)...)
	route("POST /v1/rotate", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Agent  string
			DryRun bool `json:"dry_run"`
		}
		if err := body(r, &in); err != nil {
			return nil, err
		}
		plan, err := ops.Rotate(in.Agent, in.DryRun, time.Now())
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "dry_run": in.DryRun, "closes": plan.Closes, "when": plan.When}, nil
	}, readers...)
	route("POST /v1/enable", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ Agent string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return ok, ops.Enable(in.Agent)
	}, fullOnly...)
	route("GET /v1/agents/{agent}/model", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return ops.Model(r.PathValue("agent"))
	}, readers...)
	route("POST /v1/agents/{agent}/model", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Model string
			Reset bool
		}
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if in.Reset == (in.Model != "") {
			return nil, badRequest{fmt.Errorf("model needs a model, or reset")}
		}
		return ok, ops.SetModel(r.PathValue("agent"), in.Model, time.Now())
	}, fullOnly...)
	route("POST /v1/restore", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ Archive, Agent string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if in.Archive == "" {
			return nil, badRequest{fmt.Errorf("restore needs archive")}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		return ok, ops.Restore(ctx, in.Archive, in.Agent, time.Now())
	}, fullOnly...)
	route("GET /v1/inbox", func(w http.ResponseWriter, r *http.Request) (any, error) {
		after, err := intParam(r, "after")
		if err != nil {
			return nil, err
		}
		to := r.URL.Query().Get("to") // a session's inbox; the operator's by default
		if to == "" {
			to = ledger.Operator
		}
		msgs, err := ops.InboxOf(to, after)
		if msgs == nil {
			msgs = []ledger.Relay{}
		}
		return msgs, err
	}, readers...)
	route("GET /v1/wait", long(func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		after, err := intParam(r, "after")
		if err != nil {
			return nil, err
		}
		timeout := 30 * time.Minute
		if s := q.Get("timeout"); s != "" {
			if timeout, err = time.ParseDuration(s); err != nil || timeout <= 0 {
				return nil, badRequest{fmt.Errorf("timeout %q: want e.g. 30m", s)}
			}
		}
		if q.Get("agent") == "" && q.Get("task") == "" {
			return nil, badRequest{fmt.Errorf("wait needs agent or task")}
		}
		// A member waits on a task of theirs (own needs one): by agent alone
		// is the operator's inbox.
		agent := q.Get("agent")
		if err := own(r, ops, q.Get("task")); err != nil {
			return nil, err
		}
		if scopeOf(r) == member {
			agent = ""
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		if reader := q.Get("new"); reader != "" {
			// Only what this reader has not been handed: their cursor, kept here.
			if c := callerOf(r); c != nil && scopeOf(r) == member {
				reader = memberFrom(c, reader)
			}
			return ops.WaitNew(ctx, agent, q.Get("task"), reader)
		}
		return ops.Wait(ctx, agent, q.Get("task"), after)
	}), and(readers, member)...)
	route("GET /v1/tasks/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := own(r, ops, r.PathValue("id")); err != nil {
			return nil, err
		}
		return ops.Result(r.PathValue("id"))
	}, and(readers, member)...)
	route("GET /v1/guide", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]string{"text": guide.Text()}, nil
	}, and(readers, member)...)
	route("GET /v1/events", func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		after, err := intParam(r, "after")
		if err != nil {
			return nil, err
		}
		limit, err := intParam(r, "limit")
		if err != nil {
			return nil, err
		}
		newest, err := intParam(r, "newest")
		if err != nil {
			return nil, err
		}
		// A cursor reads the ledger from its start unless told otherwise;
		// without one the window is the last two hours.
		def := "2h"
		if q.Has("after") {
			def = ""
		}
		since, err := sinceParam(r, def)
		if err != nil {
			return nil, err
		}
		eq := daemon.EventsQuery{Agent: q.Get("agent"), Since: since, After: after, Kinds: splitList(q.Get("kinds")), Limit: int(limit)}
		if newest > 0 {
			eq.Newest, eq.Limit = true, int(newest)
		}
		return ops.Events(eq)
	}, readers...)
	route("GET /v1/usage", func(w http.ResponseWriter, r *http.Request) (any, error) {
		since, err := sinceParam(r, "7d")
		if err != nil {
			return nil, err
		}
		rows, err := ops.Usage(r.URL.Query().Get("by"), since)
		if err != nil {
			return nil, badRequest{err}
		}
		return rows, nil
	}, readers...)
	route("GET /v1/history", func(w http.ResponseWriter, r *http.Request) (any, error) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		return ops.History(r.URL.Query().Get("agent"), n)
	}, readers...)
	route("GET /v1/quota", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return ops.Quota()
	}, and(readers, member)...)
	route("GET /v1/tasks", func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		n, err := intParam(r, "n")
		if err != nil {
			return nil, err
		}
		var by string
		if c := callerOf(r); c != nil && scopeOf(r) == member {
			by = c.Login // a member's own
		}
		return ops.Tasks(q.Get("open") == "1" || q.Get("open") == "true", q.Get("agent"), by, int(n))
	}, and(readers, member)...)
	route("GET /v1/activity", func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		var since time.Time
		if q.Get("since") != "" {
			var err error
			if since, err = sinceParam(r, ""); err != nil {
				return nil, err
			}
		}
		var bucket time.Duration
		if s := q.Get("bucket"); s != "" {
			var err error
			if bucket, err = time.ParseDuration(s); err != nil || bucket < time.Second {
				return nil, badRequest{fmt.Errorf("bucket %q: want e.g. 15m", s)}
			}
		}
		return ops.Activity(since, bucket, time.Now())
	}, readers...)
	route("GET /v1/today", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return ops.Today(time.Now())
	}, readers...)
	route("GET /v1/peek", func(w http.ResponseWriter, r *http.Request) (any, error) {
		agent := r.URL.Query().Get("agent")
		if agent == "" {
			return nil, badRequest{fmt.Errorf("peek needs agent")}
		}
		n, err := intParam(r, "n")
		if err != nil {
			return nil, err
		}
		// A member sees the screen of an agent at work on a task of theirs.
		if scopeOf(r) == member {
			var task string
			for _, row := range ops.Status(time.Now()) {
				if row.Agent == agent {
					task = row.Task
				}
			}
			if task == "" {
				return nil, forbidden{fmt.Errorf("%s is not working on a task of yours", agent)}
			}
			if err := own(r, ops, task); err != nil {
				return nil, forbidden{fmt.Errorf("%s is not working on a task of yours", agent)}
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		text, err := ops.Peek(ctx, agent, int(n))
		return map[string]string{"agent": agent, "text": text}, err
	}, and(readers, member)...)
	route("POST /v1/prompt", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ Agent, Text string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if in.Agent == "" || strings.TrimSpace(in.Text) == "" {
			return nil, badRequest{fmt.Errorf("prompt needs agent and text")}
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		return ok, ops.Prompt(ctx, in.Agent, in.Text)
	}, readers...)
	route("GET /v1/archives", func(w http.ResponseWriter, r *http.Request) (any, error) {
		since, err := sinceParam(r, "")
		if err != nil {
			return nil, err
		}
		return ops.Archives(since)
	}, readers...)
	route("GET /v1/archives/{id...}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return ops.Archive(r.PathValue("id"))
	}, fullOnly...)
	route("DELETE /v1/archives", func(w http.ResponseWriter, r *http.Request) (any, error) {
		older := r.URL.Query().Get("older")
		if older == "" {
			return nil, badRequest{fmt.Errorf("prune needs older, e.g. 180d (archives are otherwise kept forever)")}
		}
		before, err := ParseSince(older, time.Now())
		if err != nil {
			return nil, badRequest{err}
		}
		n, err := ops.PruneArchives(before)
		return map[string]int{"removed": n}, err
	}, fullOnly...)
	route("GET /v1/memory/recall", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operatorWork(r, ops); err != nil {
			return nil, err
		}
		q := r.URL.Query().Get("q")
		if strings.TrimSpace(q) == "" {
			return nil, badRequest{fmt.Errorf("recall needs q, what to search for")}
		}
		limit, err := intParam(r, "limit")
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		return ops.Recall(ctx, q, int(limit))
	}, full, relay)
	route("POST /v1/memory/suggest", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operatorWork(r, ops); err != nil {
			return nil, err
		}
		var in struct{ From, Text, Why string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, badRequest{fmt.Errorf("suggest needs text, the fact to keep")}
		}
		if name, ok := verifiedAgent(r); ok && scopeOf(r) == relay {
			in.From = name
		}
		if in.From == "" {
			in.From = ledger.Operator
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		to, err := ops.Suggest(ctx, in.From, in.Text, in.Why, time.Now())
		return map[string]any{"ok": err == nil, "to": to}, err
	}, full, relay)
	// Secrets: the full scope keeps and grants them; an agent reads one it is
	// granted, as the agent its identity says (never a name it sends), and
	// fleetd decides the rest from the ledger (daemon.SecretRead). A value
	// is only ever in a put's body and a read's answer.
	route("POST /v1/secrets", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Name  string
			Value []byte
		}
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&in); err != nil {
			return nil, badRequest{fmt.Errorf("bad JSON body: want name and a base64 value")} // the decoder's words may quote it
		}
		return ok, ops.SecretPut(in.Name, in.Value, time.Now())
	}, fullOnly...)
	route("GET /v1/secrets", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return ops.Secrets(time.Now())
	}, fullOnly...)
	route("POST /v1/secrets/{name}/grant", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ Task, Agent, TTL string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		var ttl time.Duration
		if in.TTL != "" {
			var err error
			if ttl, err = time.ParseDuration(in.TTL); err != nil || ttl <= 0 {
				return nil, badRequest{fmt.Errorf("ttl %q: want a duration like 2h", in.TTL)}
			}
		}
		by := ledger.Operator
		if c := callerOf(r); c != nil {
			by = c.Login
		}
		return ok, ops.SecretGrant(r.PathValue("name"), in.Task, in.Agent, by, ttl, time.Now())
	}, fullOnly...)
	route("POST /v1/secrets/{name}/revoke", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ Task, Agent string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return ok, ops.SecretRevoke(r.PathValue("name"), in.Task, in.Agent)
	}, fullOnly...)
	route("DELETE /v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return ok, ops.SecretRemove(r.PathValue("name"))
	}, fullOnly...)
	route("POST /v1/secrets/{name}/read", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operatorWork(r, ops); err != nil {
			return nil, err
		}
		agent, verified := verifiedAgent(r)
		if !verified {
			return nil, forbidden{fmt.Errorf("a secret is read by a verified agent only: this request's agent identity does not check out")}
		}
		v, err := ops.SecretRead(agent, r.PathValue("name"), time.Now())
		if err != nil {
			return nil, err
		}
		return map[string]any{"name": r.PathValue("name"), "value": v}, nil
	}, relay)
	hostRoute := func(name string, run func(ctx context.Context, hosts []string) (string, error)) {
		route("POST /v1/"+name, func(w http.ResponseWriter, r *http.Request) (any, error) {
			var in struct{ Hosts []string }
			if r.ContentLength != 0 {
				if err := body(r, &in); err != nil {
					return nil, err
				}
			}
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
			defer cancel()
			out, err := run(ctx, in.Hosts)
			if err != nil {
				return nil, fmt.Errorf("%w\n%s", err, out) // what ran and said is part of the failure
			}
			return map[string]any{"ok": true, "output": out}, nil
		}, fullOnly...)
	}
	gatewayRoutes(route, long, ops)
	hostRoute("up", ops.Up)
	hostRoute("down", ops.Down)
	hostRoute("render", func(ctx context.Context, _ []string) (string, error) { return ops.Render() })
	return withAgent(tok, ops.IsWorker, mux)
}

type agentKey struct{}

// withAgent checks an agent's key (identity) on every request that carries
// one, and refuses one that does not verify: a request either proves which
// agent it is from or does not claim to be one. The key is its container's,
// so it verifies any name of that agent's: the name must also be a worker
// fleet.yaml has (no agent-03:9 when agent-03 has two slots).
func withAgent(tok Tokens, isWorker func(string) bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get(identity.Header)
		if v == "" {
			h.ServeHTTP(w, r)
			return
		}
		name, ok := identity.Verify(tok.Full, v)
		if !ok {
			writeErr(w, http.StatusForbidden, fmt.Errorf("%s does not verify: an agent sends its own FLEET_AGENT_KEY (fleetctl render writes it)", identity.Header))
			return
		}
		if !isWorker(name) {
			writeErr(w, http.StatusForbidden, fmt.Errorf("%s verifies, but %s is no worker of fleet.yaml: a slot is <agent>:k for k up to its slots", identity.Header, name))
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, name)))
	})
}

// verifiedAgent is the agent (worker) a request proved it is from, with its
// container's key. Every route that hands an agent the operator's data asks
// it, never the name an agent sends.
func verifiedAgent(r *http.Request) (string, bool) {
	name, ok := r.Context().Value(agentKey{}).(string)
	return name, ok && name != ""
}

// provenFrom is the sender of an agent's request: the agent's verified name
// when it proved one (a different body "from" is refused), else what the body
// says, as before keys were rendered. Other scopes' names pass through.
func provenFrom(r *http.Request, asked string) (string, error) {
	if scopeOf(r) != relay {
		return asked, nil
	}
	if name, ok := verifiedAgent(r); ok {
		if asked != "" && asked != name {
			return "", forbidden{fmt.Errorf("this is %s's key: it does not speak as %s", name, asked)}
		}
		return name, nil
	}
	return asked, nil
}

// taskAssigner is who gave an open task, from the ledger.
func taskAssigner(ops Ops, task string) (string, error) {
	t, err := ops.TaskAgent(task)
	if err != nil {
		return "", err
	}
	if !t.Closed.IsZero() {
		return "", fmt.Errorf("task %s is closed", task)
	}
	return t.Assigner, nil
}

// operatorWork guards the operator's personal data (memory, secrets, the
// KARMAX gateway): the full scope may use it; an agent only while its
// current task, per the ledger, came from the operator's side
// (Ops.OperatorAssigner); a member and the console never.
func operatorWork(r *http.Request, ops Ops) error {
	switch scopeOf(r) {
	case full:
		return nil
	case relay:
	default:
		return forbidden{fmt.Errorf("the operator's personal data is not for scope %s", scopeOf(r))}
	}
	name, ok := verifiedAgent(r)
	if !ok {
		return forbidden{fmt.Errorf("an agent proves who it is with its FLEET_AGENT_KEY (%s) to use the operator's data", identity.Header)}
	}
	var task string
	for _, row := range ops.Status(time.Now()) {
		if row.Agent == name {
			task = row.Task
		}
	}
	if task == "" {
		return forbidden{fmt.Errorf("%s has no task: the operator's data is only for work the operator gave", name)}
	}
	assigner, err := taskAssigner(ops, task)
	if err != nil {
		return forbidden{fmt.Errorf("%s: %v", name, err)}
	}
	if !ops.OperatorAssigner(assigner) {
		return forbidden{fmt.Errorf("task %s came from %s, not the operator: the operator's data is not for it", task, assigner)}
	}
	return nil
}

type scopeKey struct{}

// Caller is who a request on the tailnet listener is from, set by Tailnet:
// it stands in for a token. Role is "operator" or "member".
type Caller struct {
	Login, Name, Role string
	// MaxOpen is how many open tasks a member may hold.
	MaxOpen int
}

type callerKey struct{}

func callerOf(r *http.Request) *Caller {
	c, _ := r.Context().Value(callerKey{}).(*Caller)
	return c
}

func (c *Caller) scope() scope {
	switch c.Role {
	case "operator":
		return full
	case "member":
		return member
	}
	return none
}

// own refuses a member a task someone else asked for. Everyone else may.
func own(r *http.Request, ops Ops, task string) error {
	c := callerOf(r)
	if c == nil || scopeOf(r) != member {
		return nil
	}
	if task == "" {
		return badRequest{fmt.Errorf("name the task (--task)")}
	}
	t, err := ops.TaskAgent(task)
	if err != nil {
		return err
	}
	if !ledger.OwnedBy(t.Assigner, c.Login) {
		return forbidden{fmt.Errorf("task %s is not yours", task)}
	}
	return nil
}

// memberFrom is the name a member's request is made under: their login, or a
// session of theirs (login/session), which they may name by the session alone.
// It is never someone else's.
func memberFrom(c *Caller, asked string) string {
	switch {
	case asked == "" || asked == ledger.Operator || asked == c.Login:
		return c.Login
	case ledger.OwnedBy(asked, c.Login):
		return asked
	case strings.ContainsAny(asked, "@/"):
		return c.Login // someone else's name, or a path: not theirs to use
	}
	return c.Login + "/" + asked
}

func scopeOf(r *http.Request) scope {
	s, _ := r.Context().Value(scopeKey{}).(scope)
	return s
}

// splitList is a comma list without blanks.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func intParam(r *http.Request, name string) (int64, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, badRequest{fmt.Errorf("%s %q: want a non-negative integer", name, s)}
	}
	return n, nil
}

type badRequest struct{ error }

type forbidden struct{ error }

type notFound struct{ error }

func body(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v); err != nil {
		return badRequest{fmt.Errorf("bad JSON body: %w", err)}
	}
	return nil
}

func sinceParam(r *http.Request, def string) (time.Time, error) {
	s := r.URL.Query().Get("since")
	if s == "" {
		s = def
	}
	t, err := ParseSince(s, time.Now())
	if err != nil {
		return t, badRequest{err}
	}
	return t, nil
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// ParseSince reads "30m", "2h", "7d" (ago) or an RFC 3339 time. Empty is the
// beginning of time.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil && n >= 0 {
			return now.Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return time.Time{}, fmt.Errorf("since %q: want 30m, 2h, 7d or an RFC 3339 time", s)
	}
	return now.Add(-d), nil
}
