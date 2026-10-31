package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/reflex"
	"github.com/MelloB1989/karmax/internal/sources"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// screenEvent is the agent's System One pass.
//
// A false second return means the event is finished without a model ever
// seeing it. That is the whole saving, so it is also the only branch that can
// lose work — which is why a drop has to clear a confidence bar, an operator is
// floored above it, and every failure path inside reflex returns "handle".
//
// It runs on the conversation's mailbox worker, not in the event router. The
// router delivers one event at a time, so a network call there would put every
// screening in series behind every other; here they are concurrent across
// conversations and ordered within one.
func (rt *KarmaxRuntime) screenEvent(ctx context.Context, evt bus.Event) (bus.Event, bool) {
	// Every event turn passes here, screened or not, so this is where an
	// event from a conversation a memory loop reads learns it: the turn acts
	// on it and leaves filing what it says to the loop.
	evt = rt.memorySynced.Mark(evt)
	if !rt.reflex.Available() {
		return evt, true
	}
	if reason, skip := rt.agentIgnores(evt); skip {
		// Screening something the agent discards unread is pure cost. Worse, a
		// monitored chat is wa-monitor's to judge, with far more context than
		// this has — so paying here buys a second, poorer opinion and files
		// strangers' chatter into the operator's memory on the strength of it.
		rt.log.Debug("not screening an event the agent does not handle",
			zap.String("kind", string(evt.Kind)), zap.String("reason", reason))
		return evt, true
	}
	v := rt.reflex.Screen(ctx, evt, rt.hintFor(evt))
	rt.recordVerdict(evt, v)

	switch v.Action {
	case reflex.ActionDrop:
		rt.log.Info("reflex dropped an event",
			zap.String("kind", string(evt.Kind)), zap.String("event", evt.ID),
			zap.Float64("confidence", v.Confidence), zap.String("reason", v.Reason))
		return evt, false

	case reflex.ActionRemember:
		if src, ok := sources.FromBusEvent(evt); ok && src.MemorySynced {
			// Its conversation is read into memory whole by a memory loop,
			// which files what is durable in it; a fact filed here too is the
			// same fact twice. Nothing else is asked of it, so it ends here.
			rt.log.Debug("reflex left a remembered event to its memory loop",
				zap.String("source", src.Source), zap.String("event", evt.ID))
			return evt, false
		}
		rt.rememberEvent(evt, v)
		return evt, false
	}

	// Handle, delegate and escalate all open a turn. What separates them is
	// what the agent is told, which rides along on the event.
	annotate(&evt, v)
	return evt, true
}

// agentIgnores reports events the agent will not act on whatever reflex says.
//
// Mirrors the early return in Agent.handleEvent: a message from a chat that is
// not the operator's is left to the event loops, so the turn does nothing with
// it. Screening it would decide something nobody reads.
func (rt *KarmaxRuntime) agentIgnores(evt bus.Event) (string, bool) {
	if evt.Kind != bus.EventCommsMessage {
		return "", false
	}
	chatID, _ := evt.Payload["channel_id"].(string)
	if rt.isOperatorChat(evt.AgentID, chatID) {
		return "", false
	}
	return "a monitored chat, handled by the event loops", true
}

// hintFor is what the router knows that the event does not. Operator status is
// established from the chat the message arrived on and nothing else.
func (rt *KarmaxRuntime) hintFor(evt bus.Event) reflex.Hint {
	h := reflex.Hint{}
	switch evt.Kind {
	case bus.EventCommsMessage, bus.EventCommsSent:
		chatID, _ := evt.Payload["channel_id"].(string)
		h.Channel = chatID
		h.Operator = rt.isOperatorChat(evt.AgentID, chatID)
		if name, _ := evt.Payload["sender_name"].(string); name != "" {
			h.Sender = name
		} else if name, _ := evt.Payload["chat_name"].(string); name != "" {
			h.Sender = name
		}
	default:
		// Anything the operator did not say is screened on its merits. A timer
		// or a scheduled job is the daemon talking to itself.
		h.Operator = false
		// An account's event carries its source's own urgency, which the
		// screener may not overrule: it is a fact about the event, like who
		// sent a message.
		//
		// Only for news, though: a source's backlog (its first look at an
		// account) and an item that happened hours before it arrived are
		// screened on their merits instead of floored to escalate.
		if src, ok := sources.FromBusEvent(evt); ok {
			h.Channel = src.Ref()
			h.Urgent = src.Urgent && !src.Backlog && rt.policy.Fresh(src.At, time.Now())
		}
	}
	return h
}

// isOperatorChat asks the agent, which is the only thing that knows.
//
// The operator floor is a safety property, so it is answered from one place.
// A bare WHATSAPP_OPERATOR_CHATS lookup here would miss the WHATSAPP_TARGET
// fallback the agent was configured with, and would disagree with the routing
// decision the agent itself makes about the same chat.
//
// No agent, or no answer, means the operator: an unrecognised chat is never
// silently screened away.
func (rt *KarmaxRuntime) isOperatorChat(agentID, chatID string) bool {
	a, ok := rt.agents.Get(agentID)
	if !ok || a == nil {
		return true
	}
	return a.IsOperatorChat(chatID)
}

// annotate writes the verdict onto the event so the turn can read it.
func annotate(evt *bus.Event, v reflex.Verdict) {
	if v.FailedOpen {
		return
	}
	if evt.Meta == nil {
		evt.Meta = map[string]string{}
	}
	evt.Meta[bus.MetaReflexAction] = string(v.Action)
	evt.Meta[bus.MetaReflexEffort] = string(v.Effort)
	evt.Meta[bus.MetaReflexUrgency] = fmt.Sprintf("%.2f", v.Urgency)
	evt.Meta[bus.MetaReflexRisk] = fmt.Sprintf("%.2f", v.Risk)
	evt.Meta[bus.MetaReflexReason] = v.Reason
	if v.RequireApproval {
		evt.Meta[bus.MetaReflexApproval] = "required"
	}
}

// rememberEvent files an event that says something durable but asks nothing.
//
// This is the verdict that pays for itself twice: the fact is kept, and the
// brain is never woken to keep it.
func (rt *KarmaxRuntime) rememberEvent(evt bus.Event, v reflex.Verdict) {
	agentID := evt.AgentID
	if agentID == "" {
		agentID = rt.loopDefaultAgent
	}
	entry, ok := memoryEntryForEvent(evt, v, agentID)
	if !ok {
		rt.log.Debug("reflex wanted to remember an event with nothing to store",
			zap.String("event", evt.ID))
		return
	}
	mgr := rt.memory.For(agentID, "")
	if mgr == nil {
		return
	}
	if err := mgr.Write(entry); err != nil {
		rt.log.Warn("reflex could not file a fact",
			zap.String("event", evt.ID), zap.Error(err))
		return
	}
	rt.log.Info("reflex filed an event to memory without waking the agent",
		zap.String("kind", string(evt.Kind)), zap.String("event", evt.ID))
}

// memoryEntryForEvent is the fact an event is filed as.
//
// Long-term memory keeps one document per subject, named after the entry's
// first tag, and files it by category. The tags used to be ["reflex", kind],
// so every fact reflex ever filed — a pull request, a meeting, an alarm — went
// into one document called "reflex". An account's event is filed under what it
// is ABOUT (the repository, the person), with the source and account beside it
// as cues; anything else under where it came from.
func memoryEntryForEvent(evt bus.Event, v reflex.Verdict, agentID string) (memory.MemoryEntry, bool) {
	importance := 2
	if v.Urgency >= 0.66 {
		importance = 3
	}
	entry := memory.MemoryEntry{
		ID:         uuid.New().String(),
		AgentID:    agentID,
		Role:       "system",
		Importance: importance,
		CreatedAt:  time.Now(),
	}
	if src, ok := sources.FromBusEvent(evt); ok {
		entry.Content = src.Fact()
		if entry.Content == "" {
			return memory.MemoryEntry{}, false
		}
		entry.Category = src.Category()
		entry.Tags = compactTags(src.Subject(), src.Source, src.Account, src.Type, "reflex")
		entry.OccurredAt = src.At
		if src.Urgent && entry.Importance < 3 {
			entry.Importance = 3
		}
		return entry, true
	}
	entry.Content = rememberableText(evt)
	if entry.Content == "" {
		return memory.MemoryEntry{}, false
	}
	entry.Category = "context"
	entry.Tags = compactTags(eventOrigin(evt), "reflex")
	return entry, true
}

// eventOrigin names where an event that is not an account's came from: the
// webhook route it was posted to, else its kind.
func eventOrigin(evt bus.Event) string {
	if route, _ := evt.Payload["route"].(string); strings.Trim(route, "/ ") != "" {
		return strings.Trim(route, "/ ")
	}
	return string(evt.Kind)
}

// compactTags drops empty and repeated tags, keeping order: the first is the
// subject memory files the fact under.
func compactTags(tags ...string) []string {
	out := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out
}

// rememberableText pulls the human-readable part of an event.
func rememberableText(evt bus.Event) string {
	for _, key := range []string{"content", "message", "text", "summary", "body"} {
		if s, _ := evt.Payload[key].(string); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if len(evt.Payload) == 0 {
		return ""
	}
	encoded, err := json.Marshal(evt.Payload)
	if err != nil {
		return ""
	}
	return string(evt.Kind) + ": " + string(encoded)
}

// recordVerdict persists what reflex decided, so thresholds can be retuned
// against real traffic rather than guessed at twice.
func (rt *KarmaxRuntime) recordVerdict(evt bus.Event, v reflex.Verdict) {
	if v.FailedOpen {
		return
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return
	}
	if err := rt.store.RecordReflexVerdict(evt.ID, string(evt.Kind), evt.AgentID,
		string(v.Action), string(payload)); err != nil {
		rt.log.Debug("could not record a reflex verdict", zap.Error(err))
	}
}
