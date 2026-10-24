package daemon

// A stand-in for OperatorAssigner on fleet/agent-03/v2-memory (task
// v2-memory), with that branch's signature: at merge, delete this file.

import "github.com/MelloB1989/karmax/fleet/internal/ledger"

// OperatorAssigner reports whether a task's assigner is the operator, the
// orchestrator or an operator on the tailnet — not a member, and not nobody.
func (f *Fleet) OperatorAssigner(assigner string) bool {
	switch {
	case assigner == "":
		return false
	case assigner == ledger.Operator || assigner == f.cfg.Orchestrator.Name:
		return true
	}
	return f.cfg.Tailnet.Role(assigner) == "operator"
}
