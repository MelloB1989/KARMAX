package runtime

import (
	"strings"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/integration"
)

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

// accountsFrom is what the agent is told is connected: the integration
// registry's cached statuses, read each turn without touching the network.
func accountsFrom(reg *integration.Registry) func() []agent.AccountInfo {
	return func() []agent.AccountInfo {
		if reg == nil {
			return nil
		}
		statuses := reg.Statuses()
		out := make([]agent.AccountInfo, 0, len(statuses))
		for _, st := range statuses {
			out = append(out, accountInfoOf(st))
		}
		return out
	}
}

// accountInfoOf reads one integration as an account. An integration id is
// "service" for the primary login and "service:account" for another.
func accountInfoOf(st integration.Status) agent.AccountInfo {
	service, account := st.ID, st.Account
	if i := strings.Index(service, ":"); i >= 0 {
		if account == "" {
			account = service[i+1:]
		}
		service = service[:i]
	}
	info := agent.AccountInfo{Service: service, Account: account, Name: st.Name, Connected: st.Configured}
	if st.Configured && !st.Checked.IsZero() && !st.Healthy {
		info.Problem = st.Error
		if info.Problem == "" {
			info.Problem = "failing its health check"
		}
	}
	return info
}
