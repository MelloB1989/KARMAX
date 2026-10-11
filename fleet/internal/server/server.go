// Package server is fleetd's HTTP API, which fleetctl talks to.
//
// Three token scopes: Full (you, and the orchestrator) can do everything;
// Relay (every agent container) can read status, tell, and report to the
// operator; Console (fleet-console) reads everything and prompts and rotates
// as the operator. Requests with none are refused, on every listener;
// GET /v1 lists every route and the scopes it allows.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/archive"
	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/outbound"
)

// Ops is what the API drives; *daemon.Fleet is the real one.
type Ops interface {
	Status(now time.Time) []daemon.Row
	Tell(ctx context.Context, from, to, text string, now time.Time) error
	Assign(ctx context.Context, name, task, from, brief string, now time.Time) error
	Done(name, task, outcome, branch, pr string, now time.Time) error
	Close(name, task, outcome, branch, pr string, now time.Time) error
	Rotate(name string, now time.Time) error
	Enable(name string) error
	Restore(ctx context.Context, id, onto string, now time.Time) error
	Inbox(after int64) ([]ledger.Relay, error)
	Wait(ctx context.Context, agent, task string, after int64) (daemon.WaitResult, error)
	DB() *ledger.DB
	ArchiveRoot() string
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
)

var (
	everyone = []scope{full, relay, console}
	readers  = []scope{full, console}
	fullOnly = []scope{full}
)

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
			if s == none {
				writeErr(w, http.StatusUnauthorized, fmt.Errorf("a fleet token is required (FLEET_TOKEN)"))
				return
			}
			if !slices.Contains(allowed, s) {
				writeErr(w, http.StatusForbidden, fmt.Errorf("this token (scope %s) may: %s", s, strings.Join(may[s], ", ")))
				return
			}
			v, err := h(w, r.WithContext(context.WithValue(r.Context(), scopeKey{}, s)))
			if err != nil {
				code := http.StatusInternalServerError
				if be, ok := err.(badRequest); ok {
					code, err = http.StatusBadRequest, be.error
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
	}, everyone...)
	route("GET /v1/whoami", func(w http.ResponseWriter, r *http.Request) (any, error) {
		s := scopeOf(r)
		name := ledger.Operator
		if s == relay {
			name = "agent"
		}
		return map[string]string{"scope": string(s), "name": name}, nil
	}, everyone...)
	route("GET /v1/status", func(w http.ResponseWriter, r *http.Request) (any, error) {
		out := map[string]any{"agents": ops.Status(time.Now())}
		if orch != nil {
			if q := orch(); q != nil {
				out["orchestrator"] = q
			}
		}
		return out, nil
	}, everyone...)
	route("POST /v1/tell", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ From, To, Text string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		switch scopeOf(r) {
		case console:
			in.From = ledger.Operator // the console is the operator's keyboard
		case relay:
			if in.From == ledger.Operator {
				return nil, forbidden{fmt.Errorf("only the operator's tokens speak as the operator")}
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
	route("POST /v1/done", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Agent, Task, Outcome, Branch, PR string
			KeepSession                      bool `json:"keep_session"`
		}
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if in.Agent == "" {
			return nil, badRequest{fmt.Errorf("done needs agent")}
		}
		if in.KeepSession {
			return ok, ops.Close(in.Agent, in.Task, in.Outcome, in.Branch, in.PR, time.Now())
		}
		return ok, ops.Done(in.Agent, in.Task, in.Outcome, in.Branch, in.PR, time.Now())
	}, fullOnly...)
	route("POST /v1/rotate", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ Agent string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return ok, ops.Rotate(in.Agent, time.Now())
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
		if q.Get("agent") == "" {
			return nil, badRequest{fmt.Errorf("wait needs agent")}
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		return ops.Wait(ctx, q.Get("agent"), q.Get("task"), after)
	}, readers...)
	route("GET /v1/events", func(w http.ResponseWriter, r *http.Request) (any, error) {
		since, err := sinceParam(r, "2h")
		if err != nil {
			return nil, err
		}
		return ops.DB().Events(r.URL.Query().Get("agent"), since)
	}, readers...)
	route("GET /v1/usage", func(w http.ResponseWriter, r *http.Request) (any, error) {
		since, err := sinceParam(r, "7d")
		if err != nil {
			return nil, err
		}
		by := r.URL.Query().Get("by")
		if by == "" {
			by = "agent"
		}
		rows, err := ops.DB().UsageBy(by, since)
		if err != nil {
			return nil, badRequest{err}
		}
		return rows, nil
	}, readers...)
	route("GET /v1/history", func(w http.ResponseWriter, r *http.Request) (any, error) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n <= 0 {
			n = 20
		}
		return ops.DB().History(r.URL.Query().Get("agent"), n)
	}, readers...)
	route("GET /v1/quota", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return ops.DB().LatestQuota()
	}, readers...)
	route("GET /v1/archives", func(w http.ResponseWriter, r *http.Request) (any, error) {
		since, err := sinceParam(r, "")
		if err != nil {
			return nil, err
		}
		return archive.List(ops.ArchiveRoot(), since)
	}, readers...)
	return mux
}

type scopeKey struct{}

func scopeOf(r *http.Request) scope {
	s, _ := r.Context().Value(scopeKey{}).(scope)
	return s
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
