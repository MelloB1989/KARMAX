package agent

import (
	"context"
	"strconv"
	"strings"

	"github.com/MelloB1989/karmax/internal/bus"
	"go.uber.org/zap"
)

// What System One decided, as the turn sees it.
//
// The router screened this event already, so the turn reads the verdict rather
// than forming its own opinion. An event that was never screened yields a zero
// Screening, and every branch below treats that as "do what KARMAX always did".

// Screener decides what happens to an event before a turn spends anything on
// it. A false second return means the event is finished here: it was dropped,
// or filed, and no brain is woken. Nil, or an agent with none installed,
// handles every event exactly as it did before reflex.
type Screener func(ctx context.Context, evt bus.Event) (bus.Event, bool)

// SetScreener installs System One.
func (a *Agent) SetScreener(s Screener) {
	a.mu.Lock()
	a.screener = s
	a.mu.Unlock()
}

// screen runs the installed screener, if any.
//
// It happens on the conversation's own mailbox worker rather than in the event
// router, and that placement is load-bearing: the router delivers events one at
// a time, so a network call there would put every screening in series behind
// every other. Here they run concurrently across conversations, and stay
// ordered within one.
func (a *Agent) screen(evt bus.Event) (bus.Event, bool) {
	a.mu.RLock()
	s := a.screener
	a.mu.RUnlock()
	if s == nil {
		return evt, true
	}
	return s(a.ctx, evt)
}

// Screening is the verdict carried on an event's metadata.
type Screening struct {
	Screened bool
	Action   string
	Effort   string
	Urgency  float64
	Risk     float64
	Approval bool
	Reason   string
}

// screeningOf reads the verdict off an event.
func screeningOf(evt bus.Event) Screening {
	if len(evt.Meta) == 0 {
		return Screening{}
	}
	action := evt.Meta[bus.MetaReflexAction]
	if action == "" {
		return Screening{}
	}
	s := Screening{
		Screened: true,
		Action:   action,
		Effort:   evt.Meta[bus.MetaReflexEffort],
		Approval: evt.Meta[bus.MetaReflexApproval] == "required",
		Reason:   evt.Meta[bus.MetaReflexReason],
	}
	s.Urgency, _ = strconv.ParseFloat(evt.Meta[bus.MetaReflexUrgency], 64)
	s.Risk, _ = strconv.ParseFloat(evt.Meta[bus.MetaReflexRisk], 64)
	return s
}

// brainFor picks the engine for this turn.
//
// This is where screening pays for itself. Every turn used to go to the coding
// harness whenever one was wired — minutes of latency and a full session for a
// message that needed one sentence. A verdict of "trivial" routes to the API
// session instead, which answers in a second; "heavy" and "delegate" keep the
// harness, which is what it is for.
//
// Conservative on purpose: only an explicit trivial verdict redirects, and only
// when there is an API session to redirect to. Anything unscreened, unsure or
// unrouteable lands on the engine it always used.
func (a *Agent) brainFor(evt bus.Event) Brain {
	s := screeningOf(evt)
	if !s.Screened || s.Effort != effortTrivial {
		return a.thinkingBrain()
	}
	a.mu.RLock()
	sess := a.mainSession
	hb := a.harnessBrain
	a.mu.RUnlock()
	if sess == nil || hb == nil {
		return a.thinkingBrain()
	}
	a.log.Debug("reflex routed a trivial turn to the API session",
		zap.String("kind", string(evt.Kind)), zap.String("reason", s.Reason))
	return sess
}

// effortTrivial mirrors reflex.EffortTrivial. Duplicated rather than imported
// because the value travels as a string on the wire, and the agent has no
// business depending on the screener.
const effortTrivial = "trivial"

// context renders the verdict for the model.
//
// Only the parts that change what the turn should DO are included. Telling a
// model its message scored 0.31 on urgency invites it to talk about the score.
func (s Screening) context() string {
	if !s.Screened {
		return ""
	}
	var b strings.Builder
	switch s.Action {
	case "escalate":
		b.WriteString("\n[TRIAGE] This is time-critical or needs a decision only the operator can make. Deal with it now, and if it is theirs to decide, ask them directly rather than choosing for them.\n")
	case "delegate":
		b.WriteString("\n[TRIAGE] This needs real work — shell, files, code or research. Delegate it to claude_code rather than answering from memory.\n")
	}
	if s.Approval {
		b.WriteString("[TRIAGE] Acting on this is hard to undo (money, a commitment, or a message to someone who is not the operator). Propose it for approval instead of doing it outright, unless the operator already told you to go ahead.\n")
	}
	return b.String()
}
