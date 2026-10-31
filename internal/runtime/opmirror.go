package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/api"
	"github.com/MelloB1989/karmax/internal/policy"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

const (
	mirrorJevTimeout = 15 * time.Second
	mirrorBodyLimit  = 600
)

// operatorMirror carries app notifications and approvals to the operator's
// WhatsApp DM, as far as the operating policy allows.
//
// Everything reaching here is already in the app. The mirror used to ask Jev
// whether each item was also "worth WhatsApp" and send whenever Jev could not
// answer, so a status line, a loop's digest and a quota notice all landed in
// the operator's personal chat. Now the policy decides: an approval reaches
// WhatsApp when it is on the hard floor, a notification when it is urgent, and
// nothing else does.
//
// It is also where a proposal raised for the operator meets the policy. Under
// "act, then report" a proposal that is not on the hard floor — a loop's
// suggested reply, say — needs no one's yes; it used to sit unseen in the
// app's inbox. approve carries it out through the operator's own approval
// path, and what it sends is reported after, like any turn's.
type operatorMirror struct {
	log    *zap.Logger
	send   func(text string) error
	decide func(ctx context.Context, state any, q loopkit.Questions) (*loopkit.Decision, error)
	policy policy.Policy
	// approve decides a proposal as the policy (see installOperatorMirror).
	// Nil leaves every proposal to the operator.
	approve func(id, note string) error

	mu       sync.Mutex
	inFlight map[string]int
}

func newOperatorMirror(log *zap.Logger, send func(string) error,
	decide func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error), p policy.Policy) *operatorMirror {
	return &operatorMirror{log: log, send: send, decide: decide, policy: p, inFlight: map[string]int{}}
}

// Hook is the builtin mirror hook; it never blocks the caller.
func (m *operatorMirror) Hook(ev builtin.MirrorEvent) {
	go m.handle(ev)
}

func (m *operatorMirror) handle(ev builtin.MirrorEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), mirrorJevTimeout)
	defer cancel()

	var worth bool
	if ev.Kind == builtin.MirrorApproval {
		waits := m.waitsForOperator(ctx, ev)
		if !waits && m.actOn(ev) {
			return // carried out (or already decided); its turn reports what it sent
		}
		if ev.Push != nil {
			ev.Push() // it waits in the app: the phone says so
		}
		// Off the floor it waits in the app only; WhatsApp is for the floor.
		worth = waits && m.policy.ToWhatsApp(policy.Approval)
	} else {
		worth = m.worthWhatsApp(ctx, ev)
	}
	if !worth || m.send == nil {
		return
	}
	text := mirrorText(ev)

	m.mu.Lock()
	m.inFlight[text]++
	m.mu.Unlock()
	err := m.send(text)
	m.mu.Lock()
	if m.inFlight[text]--; m.inFlight[text] <= 0 {
		delete(m.inFlight, text)
	}
	m.mu.Unlock()
	if err != nil {
		m.log.Warn("could not mirror to the operator's WhatsApp", zap.String("source", ev.Source), zap.Error(err))
	}
}

// waitsForOperator applies the operating policy to a proposal: under "ask
// first" every one is the operator's to give; under "act, then report" only
// one on the hard floor is — named so by its raiser, or placed there by Jev.
func (m *operatorMirror) waitsForOperator(ctx context.Context, ev builtin.MirrorEvent) bool {
	if !m.policy.ActFirst() || strings.TrimSpace(ev.Floor) != "" {
		return true
	}
	return m.onFloor(ctx, ev)
}

// policyNote is recorded with a proposal the policy approved: why no one was
// asked.
func (m *operatorMirror) policyNote() string {
	return "not on the hard floor (" + strings.Join(m.policy.HardFloor(), "; ") +
		"), so under the operator's policy (act, then report) it needs no approval"
}

// actOn carries out a proposal that waits for no one, through the approval
// path the operator's own yes takes. It reports true when there is nothing
// left to do: carried out, or already decided by someone else. On false the
// proposal is still pending, and waits in the app like any other.
func (m *operatorMirror) actOn(ev builtin.MirrorEvent) bool {
	if m.approve == nil || strings.TrimSpace(ev.ProposalID) == "" {
		return false
	}
	err := m.approve(ev.ProposalID, m.policyNote())
	switch {
	case err == nil:
		m.log.Info("carried out a proposal off the hard floor, under act-then-report",
			zap.String("proposal", ev.ProposalID), zap.String("source", ev.Source))
		return true
	case errors.Is(err, api.ErrAlreadyDecided):
		return true
	}
	m.log.Warn("could not carry out a proposal off the hard floor; it waits in the app",
		zap.String("proposal", ev.ProposalID), zap.Error(err))
	return false
}

// worthWhatsApp applies the operating policy to a notification.
func (m *operatorMirror) worthWhatsApp(ctx context.Context, ev builtin.MirrorEvent) bool {
	if m.policy.ToWhatsApp(policy.Notification) {
		return true
	}
	if !m.policy.ToWhatsApp(policy.Urgent) {
		return false
	}
	return ev.Urgent || m.urgent(ctx, ev)
}

// offFloorBelow is how sure Jev must be that a proposal is NOT on the hard
// floor (its "on the floor" likelihood below this) for it to be carried out
// without the operator.
const offFloorBelow = 0.3

// onFloor asks whether an approval that did not name a floor rule — a loop's
// proposal — is on the hard floor. An approval waits on a person, so one that
// cannot be classified is treated as on it: a missed hard-floor approval
// blocks work, where a spare one costs a message.
func (m *operatorMirror) onFloor(ctx context.Context, ev builtin.MirrorEvent) bool {
	if m.decide == nil {
		return true
	}
	state := map[string]any{
		"proposed_action": ev.Title,
		"detail":          clip(ev.Body, mirrorBodyLimit),
		"raised_by":       ev.Source,
	}
	dec, err := m.decide(ctx, state, loopkit.Questions{
		"floor": loopkit.Noul("An assistant acts for its operator without asking, except for these, which the operator always decides personally: "+
			strings.Join(m.policy.HardFloor(), "; ")+". Would approving this proposal let the assistant do one of them?").
			When("approving it deletes, pays for, or shares one of those things", "it is ordinary work: a message, a reply, a schedule, a change that can be undone"),
	})
	if err != nil || dec == nil {
		return true
	}
	// Off the floor means carried out without anyone's yes, so it takes a
	// clear no: an unsure answer leaves the proposal with the operator.
	return dec.Yes("floor", offFloorBelow)
}

// urgencyLevels is the length of the urgency rubric below.
const urgencyLevels = 4

// urgent asks how soon the operator must see a notification. Unanswered means
// no: the item is in the app already, and noise in the operator's WhatsApp is
// the failure they named.
func (m *operatorMirror) urgent(ctx context.Context, ev builtin.MirrorEvent) bool {
	if m.decide == nil {
		return false
	}
	state := map[string]any{
		"title":  ev.Title,
		"body":   clip(ev.Body, mirrorBodyLimit),
		"source": ev.Source,
	}
	dec, err := m.decide(ctx, state, loopkit.Questions{
		"urgency": loopkit.Score("This notice is already in the operator's phone app. How soon must the operator see it?",
			"Never needs their attention; a record of routine activity",
			"Whenever they next open the app",
			"Within the hour",
			"Immediately: waiting causes harm, a missed deadline, or leaves them unable to reach their assistant"),
	})
	if err != nil || dec == nil {
		return false
	}
	score, _ := dec.Score("urgency")
	return score/float64(urgencyLevels-1) >= m.policy.Threshold()
}

// Notifier wraps the proactive WhatsApp-to-app push so the mirror's own sends are not echoed back.
func (m *operatorMirror) Notifier(push func(target, content string)) func(target, content string) {
	return func(target, content string) {
		m.mu.Lock()
		own := m.inFlight[content] > 0
		m.mu.Unlock()
		if own {
			return
		}
		push(target, content)
	}
}

func mirrorText(ev builtin.MirrorEvent) string {
	var b strings.Builder
	if ev.Kind == builtin.MirrorApproval {
		b.WriteString("Approval needed: ")
	}
	b.WriteString(strings.TrimSpace(ev.Title))
	if body := strings.TrimSpace(clip(ev.Body, mirrorBodyLimit)); body != "" {
		b.WriteString("\n\n" + body)
	}
	if ev.Kind == builtin.MirrorApproval {
		if f := strings.TrimSpace(ev.Floor); f != "" {
			b.WriteString("\n\nAsked because it is on your hard floor: " + f + ".")
		}
		b.WriteString("\n\nProposal id: " + ev.ProposalID + "\nReply in this chat to approve or reject it.")
	}
	return b.String()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// installOperatorMirror wires the mirror and re-registers the proactive notifier behind its guard.
func (rt *KarmaxRuntime) installOperatorMirror(s *store.Store, agentID string, send func(string) error) {
	m := newOperatorMirror(rt.log.Named("opmirror"), send, rt.decide, rt.policy)
	// A proposal the policy clears goes through the operator's own approval
	// path: decided once (whoever is first), carried out as a turn of the
	// agent, its outcome recorded on the proposal, its sends reported.
	if rt.api != nil {
		m.approve = func(id, note string) error { return rt.api.DecideProposal(id, "approve", note, api.ByPolicy) }
	}
	builtin.SetMirrorHook(m.Hook)
	rt.comms.SetProactiveNotifier(m.Notifier(func(target, content string) {
		builtin.PushAppNotificationQuiet(s, agentID, "update", "Sent to "+target, clip(content, 240))
	}))
}
