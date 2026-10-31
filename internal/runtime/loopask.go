package runtime

import (
	"fmt"
	"strings"
	"sync"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A workflow's ask() is a request, with one owner.
//
// A workflow that watches a chat proxies what it cannot answer itself to the
// full agent with ask(). That is a request like any other: journalled before
// the turn starts, and continued as a task when the turn cannot finish it —
// the requester told "on it" in the chat it came from, and given the result
// there when the task is done (requests.go).
//
// The run that asked then stops answering it. A workflow's own fallback
// ladder — the cheap gateway after a failed ask, the harness after that, an
// "away" note when all else failed — was written for a request nobody else
// owned. Once a task owns it, or the full agent worked on it and could not
// finish (the operator is told), a second answer from a lighter, shorter
// model is a second owner: it duplicates work that is under way, or tells
// the chat something the real owner already knows to be false.

// loopRunState is what one loop run has settled about its request.
type loopRunState struct {
	mu     sync.Mutex
	taskID string // a task owns the request now
	failed string // the full agent worked on it and could not finish
}

func (s *loopRunState) handedTo(taskID string) {
	if s == nil || taskID == "" {
		return
	}
	s.mu.Lock()
	s.taskID = taskID
	s.mu.Unlock()
}

func (s *loopRunState) gaveUp(why string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.failed == "" {
		s.failed = why
	}
	s.mu.Unlock()
}

// refusal is the error for an attempt (what) to answer the run's request
// again after it was settled; nil while the run still owns it.
func (s *loopRunState) refusal(what string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.taskID != "":
		return fmt.Errorf("%s refused: this run's request is now task %s, which delivers its result where the request came from — nothing else in this run answers it", what, s.taskID)
	case s.failed != "":
		return fmt.Errorf("%s refused: the full agent already worked on this run's request and could not finish it (%s) — it is not retried on a lighter model, and the operator has been told", what, clip(s.failed, 200))
	}
	return nil
}

// askRequest is the request a workflow's ask makes, or nil when nothing can
// take a request over. Its answer is owed to the chat the run's message came
// from only when the operator sent that message; anyone else's request is
// reported to the operator, who decides what reaches that chat.
func (k *loopKit) askRequest(prompt string) *agent.Request {
	if k == nil || k.rt == nil || k.rt.requestOwner() == nil {
		return nil
	}
	req := agent.NewRequest("ask-"+uuid.New().String(), agent.RequestAsk)
	req.Text, req.Loop, req.AgentID = prompt, k.loopName, k.agentID
	chat := triggerChat(k.trigger)
	if chat == "" {
		req.Where = "the " + k.loopName + " workflow, for the operator"
		return req
	}
	str := func(key string) string {
		s, _ := k.trigger.Payload[key].(string)
		return strings.TrimSpace(s)
	}
	name := firstNonEmpty(str("chat_name"), str("sender_name"), chat)
	req.Asked = str("content")
	if k.triggerFromOperator() {
		req.ChannelID, req.Target = str("karmax_channel_id"), chat
		req.Where = fmt.Sprintf("the operator, in the chat %q (through the %s workflow)", name, k.loopName)
		return req
	}
	who := firstNonEmpty(str("sender_label"), str("sender_name"), "someone")
	req.Where = fmt.Sprintf("%s in the chat %q, through the %s workflow — the result goes to the operator, not to that chat", who, name, k.loopName)
	return req
}

// triggerFromOperator reports whether the run's message was the operator's:
// the channel's word for it (from the account it came from), or the chat
// being one of the operator's own.
func (k *loopKit) triggerFromOperator() bool {
	if fromOp, _ := k.trigger.Payload["sender_is_operator"].(bool); fromOp {
		return true
	}
	chat := normalizeChat(triggerChat(k.trigger))
	if chat == "" {
		return false
	}
	for _, c := range builtin.OperatorChats() {
		if normalizeChat(c) == chat {
			return true
		}
	}
	return false
}

// noteOwner records, after an ask, whether a task now owns its request —
// handed over by the turn running out of time, or by the model itself.
func (k *loopKit) noteOwner(req *agent.Request) {
	if req == nil || k.run == nil {
		return
	}
	if id := req.TaskID(); id != "" {
		k.run.handedTo(id)
		return
	}
	if task, found, _ := k.rt.store.GetTaskForRequest(req.ID); found {
		k.run.handedTo(task.ID)
	}
}

// withTaskTools lends task.create to an ask whose agent does not already
// hold it, so handing long work over is open to the model on this path too.
func (k *loopKit) withTaskTools(ag *agent.Agent, lent []tools.Tool) []tools.Tool {
	for _, m := range ag.ToolManifests() {
		if tools.CanonicalName(m.Name) == tools.CanonicalName("task.create") {
			return lent
		}
	}
	for _, t := range lent {
		if tools.CanonicalName(t.Manifest().Name) == tools.CanonicalName("task.create") {
			return lent
		}
	}
	if k.rt.tools == nil {
		return lent
	}
	if t, ok := k.rt.tools.Get("task.create"); ok {
		return append(append([]tools.Tool(nil), lent...), t)
	}
	return lent
}

// tellRequestFailed tells the requester, honestly and once, that a request
// could not be finished and is not being worked on: in the chat it came from
// when the operator asked there, otherwise on the operator's own channel (or
// the app, without one).
func (rt *KarmaxRuntime) tellRequestFailed(req *agent.Request, cause error) {
	if req == nil {
		return
	}
	text := fmt.Sprintf("I couldn't finish this: %q.\nWhat failed: %s.\nIt could not be handed over to keep working on either, so nothing more will happen on it unless it is asked again.",
		requestTitle(req), clip(rootCauseText(cause), 300))
	key := "failed:" + req.ID
	if req.ChannelID != "" && req.Target != "" {
		if _, err := rt.deliverOnce(store.Delivery{Key: key, ChannelID: req.ChannelID, Target: req.Target, Content: text}, "status"); err == nil {
			return
		}
	}
	if rt.messageOperator != nil {
		if _, err := rt.deliverWith(store.Delivery{Key: key, Content: text}, func() error { return rt.messageOperator(text) }); err == nil {
			return
		}
	}
	rt.notifyOnce(key, req.AgentID, "alert", "Couldn't finish a request", text)
	rt.log.Warn("a request could not be finished or handed over", zap.String("request", req.ID), zap.Error(cause))
}

// rootCauseText is an error in its own words, without its wrapping.
func rootCauseText(err error) string {
	if err == nil {
		return "something failed"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		return strings.TrimSpace(s[i+2:])
	}
	return s
}

// triggerChat is the chat a run's message came from, or "".
func triggerChat(t loopkit.Trigger) string {
	if t.Kind != loopkit.TriggerEvent || t.Payload == nil {
		return ""
	}
	s, _ := t.Payload["channel_id"].(string)
	return strings.TrimSpace(s)
}

// sendsTo reports whether a sending tool's input addresses chat.
func sendsTo(input map[string]any, chat string) bool {
	chat = normalizeChat(chat)
	if chat == "" {
		return false
	}
	for _, key := range []string{"to", "target", "chat", "chat_id", "jid", "channel_id", "recipient", "phone"} {
		if v, ok := input[key].(string); ok && normalizeChat(v) == chat {
			return true
		}
	}
	return false
}

// inFlightNote tells a loop's model call which requests from the run's chat
// tasks already own — so a later pass over the same chat neither answers
// them again nor asks anyone for what they need.
func (k *loopKit) inFlightNote() string {
	if k == nil || k.rt == nil || k.rt.store == nil {
		return ""
	}
	chat := triggerChat(k.trigger)
	if chat == "" {
		return ""
	}
	lines := k.rt.inFlight(chat)
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("KARMAX is already working on these requests from this chat, each as a task that will deliver its own result here:\n")
	for _, l := range lines {
		b.WriteString("- " + l + "\n")
	}
	b.WriteString("They are handled. Do not answer them, promise them again, or ask anyone for something they need; " +
		"a question about one of them is answered from this list. Deal only with what is new.\n")
	return b.String()
}
