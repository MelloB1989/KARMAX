package runtime

import (
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/harness"
)

// Where a harness turn nobody was waiting for goes.
//
// A turn that finishes after its caller gave up goes back to that caller: an
// agent's turn hands its call a late-reply handler (agent.WithLateReply),
// which the sender passes to the session as Options.OnLate. A turn no caller
// claimed — a peer's message the orchestrator answered while idle, or the rest
// of a turn whose caller asked for nothing — reaches the operator per policy
// through the agent whose session ran it. A session that is not an agent's
// belongs to its workflow, which watches harness.turn.background on the bus.

// lateReplyTo is the agent's late-reply handler in the harness's terms; nil
// when the call carried none.
func lateReplyTo(fn func(agent.HarnessTurn)) func(harness.Turn) {
	if fn == nil {
		return nil
	}
	return func(t harness.Turn) {
		calls := make([]agent.HarnessToolCall, 0, len(t.ToolCalls))
		for _, tc := range t.ToolCalls {
			calls = append(calls, agent.HarnessToolCall{Name: tc.Name, Input: tc.Input, Failed: tc.Failed})
		}
		fn(agent.HarnessTurn{Available: true, Text: t.Text, ToolCalls: calls})
	}
}

// backgroundSendWindow is how far back a background turn's sends are looked
// for when the turn does not say how long it ran.
const backgroundSendWindow = 5 * time.Minute

// reportBackgroundTurn hands a turn no caller claimed to the agent whose
// session ran it, to report to the operator per policy.
func (rt *KarmaxRuntime) reportBackgroundTurn(key string, t harness.Turn) {
	id, ok := strings.CutPrefix(key, "agent:")
	if !ok || rt.agents == nil {
		return
	}
	a, found := rt.agents.Get(id)
	if !found || a == nil {
		return
	}
	ran := t.Duration
	if ran <= 0 {
		ran = backgroundSendWindow
	}
	a.ReportBackgroundTurn(time.Now().Add(-ran).Truncate(time.Second), t.Text)
}
