package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// gateway is who may use the KARMAX gateway: the operator's side (full) and
// agents on the operator's work (relay, checked by operatorWork). Members and
// the console may not, not even to read.
var gateway = []scope{full, relay}

// gatewayRoutes are the KARMAX gateway's: the tools an agent may call, a
// call, the wait on a write's approval, and the orchestrator's decisions.
func gatewayRoutes(route func(string, func(http.ResponseWriter, *http.Request) (any, error), ...scope), long func(func(http.ResponseWriter, *http.Request) (any, error)) func(http.ResponseWriter, *http.Request) (any, error), ops Ops) {
	route("GET /v1/karmax/tools", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operatorWork(r, ops); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		return ops.KarmaxTools(ctx)
	}, gateway...)
	route("POST /v1/karmax/call", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Tool, Reason, Timeout, From string
			Args                        map[string]any
		}
		if err := body(r, &in); err != nil {
			return nil, err
		}
		if in.Tool == "" {
			return nil, badRequest{fmt.Errorf("a call needs tool")}
		}
		var timeout time.Duration
		if in.Timeout != "" {
			var err error
			if timeout, err = time.ParseDuration(in.Timeout); err != nil || timeout <= 0 {
				return nil, badRequest{fmt.Errorf("timeout %q: want a duration like 30m", in.Timeout)}
			}
		}
		who, err := requester(r, ops, in.From)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 13*time.Minute)
		defer cancel()
		return ops.KarmaxCall(ctx, who, in.Tool, in.Args, in.Reason, timeout, time.Now())
	}, gateway...)
	route("GET /v1/approvals/{id}", long(func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return nil, badRequest{fmt.Errorf("approval %q: want its number", r.PathValue("id"))}
		}
		wait := 5 * time.Minute
		if s := r.URL.Query().Get("wait"); s != "" {
			if wait, err = time.ParseDuration(s); err != nil || wait < 0 {
				return nil, badRequest{fmt.Errorf("wait %q: want e.g. 5m", s)}
			}
		}
		who, err := requester(r, ops, r.URL.Query().Get("from"))
		if err != nil {
			return nil, err
		}
		// The wait. An approved tool that starts runs to its end (AwaitApproval).
		ctx, cancel := context.WithTimeout(r.Context(), min(wait, 30*time.Minute))
		defer cancel()
		return ops.AwaitApproval(ctx, who, id)
	}), gateway...)
	route("GET /v1/approvals", func(w http.ResponseWriter, r *http.Request) (any, error) {
		n, err := intParam(r, "n")
		if err != nil {
			return nil, err
		}
		state := r.URL.Query().Get("state")
		if state == "all" {
			state = ""
		}
		return ops.Approvals(state, int(n))
	}, fullOnly...)
	decide := func(approve bool) func(w http.ResponseWriter, r *http.Request) (any, error) {
		return func(w http.ResponseWriter, r *http.Request) (any, error) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				return nil, badRequest{fmt.Errorf("approval %q: want its number", r.PathValue("id"))}
			}
			var in struct{ By, Reason string }
			if err := body(r, &in); err != nil {
				return nil, err
			}
			if !approve && strings.TrimSpace(in.Reason) == "" {
				return nil, badRequest{fmt.Errorf("a deny needs --reason: the agent is told it")}
			}
			if in.By == "" {
				in.By = ledger.Operator
			}
			if c := callerOf(r); c != nil {
				in.By = c.Login // on the tailnet, who Tailscale says
			}
			return ops.Decide(id, in.By, approve, in.Reason, time.Now())
		}
	}
	route("POST /v1/approvals/{id}/approve", decide(true), fullOnly...)
	route("POST /v1/approvals/{id}/deny", decide(false), fullOnly...)
}

// requester is who calls the gateway, after operatorWork lets them: an agent
// by its verified name, on its current task by the ledger — never a name or
// task from the request — or a full-scope caller by the name it gives.
func requester(r *http.Request, ops Ops, from string) (daemon.Requester, error) {
	if err := operatorWork(r, ops); err != nil {
		return daemon.Requester{}, err
	}
	if scopeOf(r) == relay {
		name, _ := verifiedAgent(r)
		return daemon.Requester{Name: name, Task: agentTask(ops, name)}, nil
	}
	if c := callerOf(r); c != nil {
		from = c.Login
	}
	if from == "" {
		from = ledger.Operator
	}
	return daemon.Requester{Name: from}, nil
}

// agentTask is the open task of a worker, from fleetd's roster: each slot
// of an agent (agent-03:2) is a row with a task of its own.
func agentTask(ops Ops, name string) string {
	for _, row := range ops.Status(time.Now()) {
		if row.Agent == name {
			return row.Task
		}
	}
	return ""
}
