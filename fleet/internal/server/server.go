// Package server is fleetd's HTTP API, which fleetctl talks to.
//
// Two token scopes: Full (you, and the orchestrator in karmax-brain) can do
// everything; Relay (every agent container) can only tell and read status.
// Requests with neither are refused, on every listener.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
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
	Assign(name, task string, now time.Time) error
	Done(name, task, outcome, branch, pr string, now time.Time) error
	Rotate(name string, now time.Time) error
	Enable(name string) error
	Restore(ctx context.Context, id, onto string, now time.Time) error
	DB() *ledger.DB
	ArchiveRoot() string
}

// Tokens are the two bearer tokens.
type Tokens struct{ Full, Relay string }

type scope int

const (
	none scope = iota
	relay
	full
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
	}
	return none
}

// Handler is the API. orch, when set, reports the orchestrator's own quota.
func Handler(ops Ops, tok Tokens, orch func() *outbound.Quota) http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, need scope, h func(w http.ResponseWriter, r *http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			switch s := tok.scope(r); {
			case s == none:
				writeErr(w, http.StatusUnauthorized, fmt.Errorf("a fleet token is required (FLEET_TOKEN)"))
				return
			case s < need:
				writeErr(w, http.StatusForbidden, fmt.Errorf("this token may only tell and read status"))
				return
			}
			v, err := h(w, r)
			if err != nil {
				code := http.StatusInternalServerError
				if be, ok := err.(badRequest); ok {
					code, err = http.StatusBadRequest, be.error
				}
				writeErr(w, code, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		})
	}
	ok := map[string]bool{"ok": true}

	route("GET /v1/status", relay, func(w http.ResponseWriter, r *http.Request) (any, error) {
		out := map[string]any{"agents": ops.Status(time.Now())}
		if orch != nil {
			if q := orch(); q != nil {
				out["orchestrator"] = q
			}
		}
		return out, nil
	})
	route("POST /v1/tell", relay, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ From, To, Text string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if in.From == "" || in.To == "" || strings.TrimSpace(in.Text) == "" {
			return nil, badRequest{fmt.Errorf("tell needs from, to and text")}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		return ok, ops.Tell(ctx, in.From, in.To, in.Text, time.Now())
	})
	route("POST /v1/assign", full, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ Agent, Task string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if in.Agent == "" || in.Task == "" {
			return nil, badRequest{fmt.Errorf("assign needs agent and task")}
		}
		return ok, ops.Assign(in.Agent, in.Task, time.Now())
	})
	route("POST /v1/done", full, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ Agent, Task, Outcome, Branch, PR string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if in.Agent == "" {
			return nil, badRequest{fmt.Errorf("done needs agent")}
		}
		return ok, ops.Done(in.Agent, in.Task, in.Outcome, in.Branch, in.PR, time.Now())
	})
	route("POST /v1/rotate", full, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ Agent string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return ok, ops.Rotate(in.Agent, time.Now())
	})
	route("POST /v1/enable", full, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ Agent string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return ok, ops.Enable(in.Agent)
	})
	route("POST /v1/restore", full, func(w http.ResponseWriter, r *http.Request) (any, error) {
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
	})
	route("GET /v1/events", full, func(w http.ResponseWriter, r *http.Request) (any, error) {
		since, err := sinceParam(r, "2h")
		if err != nil {
			return nil, err
		}
		return ops.DB().Events(r.URL.Query().Get("agent"), since)
	})
	route("GET /v1/usage", full, func(w http.ResponseWriter, r *http.Request) (any, error) {
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
	})
	route("GET /v1/history", full, func(w http.ResponseWriter, r *http.Request) (any, error) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n <= 0 {
			n = 20
		}
		return ops.DB().History(r.URL.Query().Get("agent"), n)
	})
	route("GET /v1/quota", full, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return ops.DB().LatestQuota()
	})
	route("GET /v1/archives", full, func(w http.ResponseWriter, r *http.Request) (any, error) {
		since, err := sinceParam(r, "")
		if err != nil {
			return nil, err
		}
		return archive.List(ops.ArchiveRoot(), since)
	})
	return mux
}

type badRequest struct{ error }

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
