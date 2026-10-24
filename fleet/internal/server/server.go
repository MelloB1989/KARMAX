// Package server is fleetd's HTTP API, which fleetctl talks to.
//
// Three token scopes: Full (you, and the orchestrator) can do everything;
// Relay (every agent container) can read status, tell, and report to the
// operator; Console (fleet-console) reads everything and prompts and rotates
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
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/archive"
	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/filexfer"
	"github.com/MelloB1989/karmax/fleet/internal/guide"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/outbound"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// Ops is what the API drives; *daemon.Fleet is the real one.
type Ops interface {
	Status(now time.Time) []daemon.Row
	Tell(ctx context.Context, from, to, text string, now time.Time) error
	Assign(ctx context.Context, name, task, from, brief string, now time.Time) error
	Followup(ctx context.Context, task, from, text string, now time.Time) error
	Brief(ctx context.Context, task, from, repo, brief string, now time.Time) error
	Cancel(ctx context.Context, task, from, reason string, now time.Time) error
	Pick(repo string, now time.Time) daemon.PickReply
	Run(ctx context.Context, in daemon.RunRequest, now time.Time) (daemon.RunReply, error)
	Done(name, task, outcome, branch, pr string, now time.Time) error
	Close(name, task, outcome, branch, pr string, now time.Time) error
	Rotate(name string, dryRun bool, now time.Time) (daemon.RotatePlan, error)
	Enable(name string) error
	Restore(ctx context.Context, id, onto string, now time.Time) error
	Inbox(after int64) ([]ledger.Relay, error)
	Wait(ctx context.Context, agent, task string, after int64) (daemon.WaitResult, error)
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
	PutFiles(ctx context.Context, agent, dir string, body io.Reader) (int64, error)
	GetFiles(ctx context.Context, agent, path string, w io.Writer) error
	Rsh(ctx context.Context, agent string, argv []string, stdin io.Reader, stdout io.Writer) (target.Result, error)
	Archives(since time.Time) ([]*archive.Manifest, error)
	Archive(id string) (*daemon.ArchiveDetail, error)
	PruneArchives(before time.Time) (int, error)
	Render() (string, error)
	Up(ctx context.Context, hosts []string) (string, error)
	Down(ctx context.Context, hosts []string) (string, error)
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
	mux := http.NewServeMux()
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
			if _, done := v.(streamed); done && err == nil {
				return // the handler wrote the response itself
			}
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
				if errors.Is(err, daemon.ErrNoTask) {
					err = notFound{err}
				}
				if errors.Is(err, daemon.ErrBadInput) {
					code = http.StatusBadRequest
				}
				if errors.Is(err, filexfer.ErrTooLarge) {
					code = http.StatusRequestEntityTooLarge
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
		var in struct{ From, To, Text string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		switch scopeOf(r) {
		case console:
			in.From = ledger.Operator // the console is the operator's keyboard
		case relay:
			if ledger.IsPerson(in.From) {
				return nil, forbidden{fmt.Errorf("an agent does not speak as %s: only that person does", in.From)}
			}
		}
		if in.From == "" || in.To == "" || strings.TrimSpace(in.Text) == "" {
			return nil, badRequest{fmt.Errorf("tell needs from, to and text")}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		return ok, ops.Tell(ctx, in.From, in.To, in.Text, time.Now())
	}, everyone...)
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
			Brief, Repo, Agent, From, Task, Queue string
			Attach                                bool
		}
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Brief) == "" {
			return nil, badRequest{fmt.Errorf("run needs a brief")}
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
			in.From, limit = c.Login, c.MaxOpen // a member runs as themselves, and only so many
		}
		// A queued run holds the request open for the queue time.
		ctx, cancel := context.WithTimeout(r.Context(), queue+3*time.Minute)
		defer cancel()
		return ops.Run(ctx, daemon.RunRequest{Brief: in.Brief, Repo: in.Repo, Agent: in.Agent, From: in.From,
			Task: in.Task, Queue: queue, Limit: limit, Attach: in.Attach}, time.Now())
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
			in.From = c.Login
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
			in.From = c.Login
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		return ok, ops.Cancel(ctx, r.PathValue("id"), in.From, in.Reason, time.Now())
	}, and(fullOnly, member)...)
	route("POST /v1/tasks/{id}/brief", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ From, Repo, Brief string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if err := own(r, ops, r.PathValue("id")); err != nil {
			return nil, err
		}
		if c := callerOf(r); c != nil && scopeOf(r) == member {
			in.From = c.Login
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		return ok, ops.Brief(ctx, r.PathValue("id"), in.From, in.Repo, in.Brief, time.Now())
	}, and(fullOnly, member)...)
	route("PUT /v1/agents/{agent}/files", func(w http.ResponseWriter, r *http.Request) (any, error) {
		agent := r.PathValue("agent")
		if err := holds(r, ops, agent); err != nil {
			return nil, err
		}
		n, err := ops.PutFiles(r.Context(), agent, r.URL.Query().Get("path"), r.Body)
		return map[string]any{"ok": true, "bytes": n}, err
	}, and(fullOnly, member)...)
	route("GET /v1/agents/{agent}/files", func(w http.ResponseWriter, r *http.Request) (any, error) {
		agent := r.PathValue("agent")
		if err := holds(r, ops, agent); err != nil {
			return nil, err
		}
		w.Header().Set("Trailer", "X-Fleet-Error")
		sw := &startWriter{w: w, contentType: "application/x-tar"}
		err := ops.GetFiles(r.Context(), agent, r.URL.Query().Get("path"), sw)
		if err != nil && !sw.started {
			return nil, err
		}
		if err != nil { // the status is sent: the trailer says why the stream stops
			w.Header().Set("X-Fleet-Error", oneLine(err.Error()))
		}
		sw.start()
		return streamed{}, nil
	}, and(fullOnly, member)...)
	route("POST /v1/agents/{agent}/rsh", func(w http.ResponseWriter, r *http.Request) (any, error) {
		// Both ways at once: the command reads the request while it writes
		// the response.
		_ = http.NewResponseController(w).EnableFullDuplex()
		w.Header().Set("Trailer", "X-Fleet-Exit, X-Fleet-Stderr, X-Fleet-Error")
		sw := &startWriter{w: w, contentType: "application/octet-stream", flush: true}
		res, err := ops.Rsh(r.Context(), r.PathValue("agent"), r.URL.Query()["arg"], r.Body, sw)
		if err != nil && !sw.started {
			return nil, err
		}
		if err != nil {
			w.Header().Set("X-Fleet-Error", oneLine(err.Error()))
		}
		sw.start()
		w.Header().Set("X-Fleet-Exit", strconv.Itoa(res.Exit))
		w.Header().Set("X-Fleet-Stderr", url.QueryEscape(res.Stderr))
		return streamed{}, nil
	}, fullOnly...)
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
		msgs, err := ops.Inbox(after)
		if msgs == nil {
			msgs = []ledger.Relay{}
		}
		return msgs, err
	}, readers...)
	route("GET /v1/wait", func(w http.ResponseWriter, r *http.Request) (any, error) {
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
		return ops.Wait(ctx, agent, q.Get("task"), after)
	}, and(readers, member)...)
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
		if err := holds(r, ops, agent); err != nil {
			return nil, err
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
	hostRoute("up", ops.Up)
	hostRoute("down", ops.Down)
	hostRoute("render", func(ctx context.Context, _ []string) (string, error) { return ops.Render() })
	return mux
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
	if t.Assigner != c.Login {
		return forbidden{fmt.Errorf("task %s is not yours", task)}
	}
	return nil
}

// holds refuses a member an agent that is not at work on a task of theirs.
// Everyone else may reach any agent.
func holds(r *http.Request, ops Ops, agent string) error {
	if scopeOf(r) != member {
		return nil
	}
	var task string
	for _, row := range ops.Status(time.Now()) {
		if row.Agent == agent {
			task = row.Task
		}
	}
	if task == "" {
		return forbidden{fmt.Errorf("%s is not working on a task of yours", agent)}
	}
	if err := own(r, ops, task); err != nil {
		return forbidden{fmt.Errorf("%s is not working on a task of yours", agent)}
	}
	return nil
}

// streamed is a handler's answer when it has written the response itself.
type streamed struct{}

// startWriter sends the status and headers with the first byte written, so
// that a failure before any can still be an error response. flush sends
// each write on its way, for a stream both ways.
type startWriter struct {
	w           http.ResponseWriter
	contentType string
	flush       bool
	started     bool
}

func (s *startWriter) start() {
	if !s.started {
		s.started = true
		s.w.Header().Set("Content-Type", s.contentType)
		s.w.WriteHeader(http.StatusOK)
	}
}

func (s *startWriter) Write(p []byte) (int, error) {
	s.start()
	n, err := s.w.Write(p)
	if s.flush && err == nil {
		_ = http.NewResponseController(s.w).Flush()
	}
	return n, err
}

// oneLine is text fit for a header value.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
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
