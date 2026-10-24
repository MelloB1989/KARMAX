package server

// A stand-in for the verified agent identity on fleet/agent-03/v2-memory
// (task v2-memory), with that branch's signatures: at merge, delete this file
// and identity_stub_test.go, and the gateway uses the real ones. Until then
// no relay request is verified, so agents cannot use the gateway: it fails
// closed.

import (
	"fmt"
	"net/http"
)

// stubVerified stands in for the route middleware's check of X-Fleet-Agent;
// identity_stub_test.go replaces it.
var stubVerified = func(*http.Request) (string, bool) { return "", false }

// verifiedAgent is the agent (worker) name a relay request proved with its
// X-Fleet-Agent header; ok false when it sent none.
func verifiedAgent(r *http.Request) (name string, ok bool) { return stubVerified(r) }

// operatorWork is the rule for the operator's personal data (memory, secrets,
// the KARMAX gateway): full scope may; member and console scopes may not; an
// agent may only while its current task, by the ledger, was assigned by the
// operator, an operator's session or the orchestrator.
func operatorWork(r *http.Request, ops Ops) error {
	switch scopeOf(r) {
	case full:
		return nil
	case relay:
	default:
		return forbidden{fmt.Errorf("the operator's own data is not open to this caller")}
	}
	name, ok := verifiedAgent(r)
	if !ok {
		return forbidden{fmt.Errorf("an agent proves who it is (X-Fleet-Agent) to reach the operator's data")}
	}
	task := agentTask(ops, name)
	if task == "" {
		return forbidden{fmt.Errorf("%s has no open task: the operator's data is for work the operator gave", name)}
	}
	t, err := ops.TaskAgent(task)
	if err != nil {
		return err
	}
	if !t.Closed.IsZero() || !ops.OperatorAssigner(t.Assigner) {
		return forbidden{fmt.Errorf("task %s was not assigned by the operator or the orchestrator: the operator's data is not open to it", task)}
	}
	return nil
}

