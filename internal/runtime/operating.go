package runtime

import "github.com/MelloB1989/karmax/internal/agent"

// applyOperatingPolicy gives an agent the operating policy it acts under and
// the way to report to the operator's WhatsApp after a turn.
//
// messageOperator is nil when no WhatsApp channel or operator target is
// configured; the agent then keeps its reports in the app.
func (rt *KarmaxRuntime) applyOperatingPolicy(a *agent.Agent) {
	a.SetPolicy(rt.policy)
	if rt.messageOperator != nil {
		a.SetOperatorReport(rt.messageOperator)
	}
}
