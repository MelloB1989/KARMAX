package agent

import (
	"context"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"go.uber.org/zap"
)

// Late replies: a turn that finishes after its caller gave up.
//
// A harness turn has a deadline. When it passes, the agent stops waiting and
// the operator is told the turn failed — but the session may finish the turn
// anyway, and what it said then used to be published as a background turn
// nobody read. Each turn now hands its harness call a handler for the rest of
// it (WithLateReply), which delivers it to the conversation the turn was for,
// per the operating policy. A turn the session ran that no conversation
// claims at all is reported to the operator per policy (ReportBackgroundTurn).

type lateReplyKey struct{}

// WithLateReply carries the handler for the rest of a turn that finishes after
// the call carrying ctx stopped waiting for it. The harness sender reads it
// (LateReplyFrom) and hands it to the session.
func WithLateReply(ctx context.Context, fn func(HarnessTurn)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, lateReplyKey{}, fn)
}

// LateReplyFrom is the late-reply handler a turn's call carries, or nil.
func LateReplyFrom(ctx context.Context) func(HarnessTurn) {
	fn, _ := ctx.Value(lateReplyKey{}).(func(HarnessTurn))
	return fn
}

// lateHeader opens the report of an urgent event's turn that finished late:
// a follow-up to the page its failure already sent, not a second page.
const lateHeader = "Finished late — "

// deliverLateReply takes the rest of a turn for evt, begun at started, that
// finished after the agent stopped waiting for it, to where the turn's answer
// would have gone. answered: the turn was answered anyway (the fallback took
// it), so this is not a second answer — only what it sent is reported.
func (a *Agent) deliverLateReply(evt bus.Event, started time.Time, prompt string, t HarnessTurn, answered bool) {
	text := strings.TrimSpace(cleanOutboundResponse(t.Text))
	a.log.Info("a turn finished after its caller gave up; delivering it to its conversation",
		zap.String("event", evt.ID), zap.String("kind", string(evt.Kind)),
		zap.Int("len", len(text)), zap.Bool("already_answered", answered))
	// What it did is recent action like any turn's, so the next turn sees it.
	for _, tc := range harnessRecords(t.ToolCalls) {
		a.publish(bus.NewEvent(bus.EventToolCalled, a.def.ID, map[string]any{
			"tool": tools.CanonicalName(tc.Name), "input": tc.Input,
		}))
	}
	if answered {
		a.sendReport(a.reportFor(bus.Event{Kind: bus.EventHarnessBackgroundTurn, AgentID: a.def.ID}, started, "", lateHeader))
		return
	}

	// A message's answer goes to its chat, unless the chat has been answered
	// since the turn began — by the turn itself, through a tool.
	if evt.Kind == bus.EventCommsMessage && a.commsSend != nil && text != "" {
		channelID, _ := evt.Payload["karmax_channel_id"].(string)
		target, _ := evt.Payload["channel_id"].(string)
		if channelID != "" && !a.repliedDuringTurn(target, started) {
			if err := a.commsSend(channelID, target, text); err != nil {
				a.log.Warn("could not deliver a late reply", zap.String("channel", channelID), zap.Error(err))
			}
		}
	}
	if text != "" {
		a.recordConversation(a.ctx, evt, prompt, text)
		a.publish(bus.NewEvent(bus.EventAgentMessage, a.def.ID, map[string]any{
			"response": text, "trigger_event": evt.ID, "late": true,
		}))
	}
	// An event's turn is reported as it would have been: an urgent one's
	// result follows up its page; what was sent as the operator is listed.
	a.sendReport(a.reportFor(evt, started, text, lateHeader))
}

// publish puts an event on the bus, when the agent has one.
func (a *Agent) publish(e bus.Event) {
	if a.bus != nil {
		_ = a.bus.Publish(e)
	}
}

// lateReplyFor is the handler a turn for evt hands its harness call.
// answered reports, when the rest arrives, whether the turn was answered
// anyway.
func (a *Agent) lateReplyFor(evt bus.Event, started time.Time, prompt string, answered func() bool) func(HarnessTurn) {
	return func(t HarnessTurn) { a.deliverLateReply(evt, started, prompt, t, answered()) }
}

// ReportBackgroundTurn tells the operator, per the policy, about a turn this
// agent's session ran that no conversation was waiting for — a peer's message
// it answered, or the rest of a turn nobody claimed. Its words are kept in the
// app (where the policy's urgency check decides whether they also reach
// WhatsApp), and what it sent as the operator since `since` is reported like
// any turn's.
func (a *Agent) ReportBackgroundTurn(since time.Time, text string) {
	text = strings.TrimSpace(cleanOutboundResponse(text))
	if text != "" && a.store != nil {
		// Titled by what it says, and of a kind never folded into an earlier
		// notice with the same title: each such turn is its own news.
		builtin.PushAppNotification(a.store, a.def.ID, "info", truncateStr(firstLineOf(text), 80), truncateStr(text, 1500))
	}
	a.deliverTurnReport(bus.Event{Kind: bus.EventHarnessBackgroundTurn, AgentID: a.def.ID}, since, text)
}
