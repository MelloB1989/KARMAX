package agent

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/policy"
	"github.com/MelloB1989/karmax/internal/sources"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"go.uber.org/zap"
)

// The operating policy, as the agent applies it.
//
// The operator decided: act freely, including writing to people as them, and
// tell them after — on WhatsApp only for hard-floor approvals, urgent items,
// and one line per turn saying what was sent as them. The model is told the
// policy every turn (buildPolicyContext), and the reporting half is done here,
// in code, after the turn: an alarm reaching the operator must not depend on a
// model remembering to send it, and one message per turn cannot be promised by
// a model that sends as it goes.

// policyState is what the agent acts for the operator under: the policy, what
// it has reported, and the accounts it acts through (accounts.go).
type policyState struct {
	mu     sync.Mutex
	policy *policy.Policy
	report func(text string) error
	// reported holds the outbound messages already listed for the operator, so
	// two turns ending close together do not both list the same send.
	reported map[string]time.Time
	// accounts reads what is connected; seen is the source accounts events
	// have arrived from, with when.
	accounts func() []AccountInfo
	seen     map[string]time.Time
}

// SetPolicy installs the operating policy. Without one the agent runs on the
// operator's decided default.
func (a *Agent) SetPolicy(p policy.Policy) {
	a.ops.mu.Lock()
	a.ops.policy = &p
	a.ops.mu.Unlock()
}

// SetOperatorReport installs the sender for the per-turn report to the
// operator's WhatsApp.
func (a *Agent) SetOperatorReport(fn func(text string) error) {
	a.ops.mu.Lock()
	a.ops.report = fn
	a.ops.mu.Unlock()
}

func (a *Agent) operatingPolicy() policy.Policy {
	a.ops.mu.Lock()
	defer a.ops.mu.Unlock()
	if a.ops.policy == nil {
		return policy.Default()
	}
	return *a.ops.policy
}

// buildPolicyContext is the policy as the model reads it on every turn.
func (a *Agent) buildPolicyContext() string { return a.operatingPolicy().Brief() }

// sentLinePrefix marks a send the model declares in its final reply, for what
// KARMAX cannot see itself: a GitHub comment, an email, a post.
const sentLinePrefix = "SENT:"

// deliverTurnReport sends the turn's report, if the policy calls for one, and
// keeps it on record in the app.
func (a *Agent) deliverTurnReport(evt bus.Event, since time.Time, response string) {
	text := a.turnReport(evt, since, response)
	if text == "" {
		return
	}
	a.ops.mu.Lock()
	send := a.ops.report
	a.ops.mu.Unlock()
	if a.store != nil {
		builtin.PushAppNotificationQuiet(a.store, a.def.ID, "report", "What KARMAX did", text)
	}
	if send == nil {
		return
	}
	if err := send(text); err != nil {
		a.log.Warn("could not send the turn report to the operator", zap.Error(err))
	}
}

// turnReport is what the operator is told after a turn: the event, if it was
// urgent, with what was done about it; and what was sent as them. Empty when
// there is nothing the policy sends.
func (a *Agent) turnReport(evt bus.Event, since time.Time, response string) string {
	// The operator's own conversation reports itself: they are in it.
	if evt.Kind == bus.EventCommsMessage {
		if chat, _ := evt.Payload["channel_id"].(string); a.isFromOperator(chat) {
			return ""
		}
	}
	p := a.operatingPolicy()
	var b strings.Builder
	if p.ToWhatsApp(policy.Urgent) && turnIsUrgent(evt, p) {
		b.WriteString("⚠️ Urgent — " + eventHeadline(evt) + "\n")
		if done := strings.TrimSpace(withoutSentLines(response)); done != "" {
			b.WriteString(truncateStr(done, 700) + "\n")
		}
	}
	if p.ToWhatsApp(policy.Sent) {
		sent := append(a.sentSince(since), declaredSends(response)...)
		if len(sent) > 0 {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("Sent as you:\n")
			for _, s := range sent {
				b.WriteString("• " + s + "\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// turnIsUrgent: the source flagged it, or the screener scored it at the
// policy's threshold.
func turnIsUrgent(evt bus.Event, p policy.Policy) bool {
	flagged := false
	if src, ok := sources.FromBusEvent(evt); ok {
		flagged = src.Urgent
	}
	s := screeningOf(evt)
	return p.IsUrgent(flagged, s.Urgency)
}

// eventHeadline names an event in one line.
func eventHeadline(evt bus.Event) string {
	if src, ok := sources.FromBusEvent(evt); ok {
		what := src.Title
		if what == "" {
			what = truncateStr(src.Summary, 120)
		}
		return fmt.Sprintf("%s (%s · %s)", what, src.Source, src.Account)
	}
	for _, k := range []string{"title", "summary", "content", "message"} {
		if s, _ := evt.Payload[k].(string); strings.TrimSpace(s) != "" {
			return truncateStr(strings.TrimSpace(s), 120)
		}
	}
	if route, _ := evt.Payload["route"].(string); route != "" {
		return string(evt.Kind) + " " + route
	}
	return string(evt.Kind)
}

// sentSince lists what went to other people since the turn began, once each.
// Loop and daemon posts are not the agent's, the slow-turn ack is not a
// message, and the operator's own chats are not "other people".
func (a *Agent) sentSince(since time.Time) []string {
	if a.store == nil {
		return nil
	}
	rows, err := a.store.ListOutboundSince(since, 50)
	if err != nil {
		a.log.Warn("could not list what this turn sent", zap.Error(err))
		return nil
	}
	a.ops.mu.Lock()
	defer a.ops.mu.Unlock()
	if a.ops.reported == nil {
		a.ops.reported = map[string]time.Time{}
	}
	for id, at := range a.ops.reported {
		if time.Since(at) > time.Hour {
			delete(a.ops.reported, id)
		}
	}
	var out []string
	for _, r := range rows {
		if !comms.IsAnswerProducer(r.SenderID) || strings.TrimSpace(r.Content) == ackMessage {
			continue
		}
		if a.isKnownOperatorChat(r.ChannelID) || a.ops.reported[r.ID] != (time.Time{}) {
			continue
		}
		a.ops.reported[r.ID] = time.Now()
		out = append(out, fmt.Sprintf("%s → %s: “%s”", channelLabel(r.ChannelType), a.recipientName(r.ChannelID),
			truncateStr(strings.Join(strings.Fields(r.Content), " "), 90)))
	}
	return out
}

// isKnownOperatorChat is a strict "this chat is the operator's": unlike
// isFromOperator, an unknown chat is not assumed to be theirs.
func (a *Agent) isKnownOperatorChat(chatID string) bool {
	n := normalizeChat(chatID)
	return n != "" && a.operatorChats[n]
}

// recipientName is who a chat is, as the operator would say it: a saved
// contact, else the name the chat last arrived under, else its id.
func (a *Agent) recipientName(chatID string) string {
	if digits := normalizeChat(chatID); digits != "" && strings.Trim(digits, "0123456789") == "" {
		if name := a.store.LookupContactName(digits); name != "" {
			return name
		}
	}
	if rows, err := a.store.ListChannelMessages(chatID, 20); err == nil {
		for _, r := range rows {
			if strings.EqualFold(r.Direction, "inbound") && strings.TrimSpace(r.SenderName) != "" {
				return r.SenderName
			}
		}
	}
	return chatID
}

func channelLabel(t string) string {
	switch strings.ToLower(t) {
	case "whatsapp":
		return "WhatsApp"
	case "":
		return "message"
	default:
		return strings.ToUpper(t[:1]) + t[1:]
	}
}

// declaredSends are the SENT: lines the model wrote for sends KARMAX cannot
// see itself.
func declaredSends(response string) []string {
	var out []string
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*• "))
		if len(line) > len(sentLinePrefix) && strings.EqualFold(line[:len(sentLinePrefix)], sentLinePrefix) {
			if s := strings.TrimSpace(line[len(sentLinePrefix):]); s != "" {
				out = append(out, truncateStr(s, 200))
			}
		}
	}
	return out
}

// withoutSentLines is the reply minus its SENT: declarations, which are listed
// separately.
func withoutSentLines(response string) string {
	var keep []string
	for _, line := range strings.Split(response, "\n") {
		l := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*• "))
		if len(l) >= len(sentLinePrefix) && strings.EqualFold(l[:len(sentLinePrefix)], sentLinePrefix) {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// buildOperatingContext is what the agent needs to act for the operator, beyond
// who they are: the policy it acts under, and the accounts it acts through.
func (a *Agent) buildOperatingContext() string {
	return a.buildPolicyContext() + a.buildAccountsContext()
}

// operatingContextFor is buildOperatingContext for an event's turn, noting the
// account an event from one of the operator's accounts arrived from.
func (a *Agent) operatingContextFor(evt bus.Event) string {
	if src, ok := sources.FromBusEvent(evt); ok {
		a.noteSourceAccount(src, time.Now())
	}
	return a.buildOperatingContext()
}
