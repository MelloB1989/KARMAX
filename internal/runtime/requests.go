package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Requests a turn could not finish, owned by the task runner.
//
// The agent decides that a request goes on (agent/request.go); this is where
// it goes: a task that continues the same request — what was asked, the
// conversation's context, what the turn already did and where the answer is
// owed — created atomically with closing the request's turn
// (store.TakeOverRequest), started at once, and acknowledged to the
// requester. The result goes back to the same place when the task finishes
// (taskrunner.go), recorded so it is delivered once across restarts.

// taskOwner is the runtime as agent.TaskOwner.
type taskOwner struct{ rt *KarmaxRuntime }

var _ agent.TaskOwner = taskOwner{}

func (o taskOwner) HandOff(_ context.Context, h agent.Handoff) (agent.HandoffResult, error) {
	return o.rt.handOff(h)
}

func (o taskOwner) LateResult(taskID string, t agent.HarnessTurn) { o.rt.lateTaskResult(taskID, t) }

func (o taskOwner) InFlight(target string) []string { return o.rt.inFlight(target) }

// requestOwner is what the agents and the loop host hand requests to; nil
// when no harness can work a task, in which case requests run as they did.
func (rt *KarmaxRuntime) requestOwner() agent.TaskOwner {
	if rt == nil || rt.store == nil || rt.harness == nil {
		return nil
	}
	return taskOwner{rt: rt}
}

// maxTaskContext bounds the conversation context a task's brief carries.
const maxTaskContext = 16000

// handOff makes a task the owner of a request and tells the requester.
func (rt *KarmaxRuntime) handOff(h agent.Handoff) (agent.HandoffResult, error) {
	req := h.Request
	if req == nil || strings.TrimSpace(req.ID) == "" {
		return agent.HandoffResult{}, errors.New("no request to hand over")
	}
	if rt.store == nil {
		return agent.HandoffResult{}, errors.New("no store to own the request")
	}
	agentID := req.AgentID
	if agentID == "" {
		agentID = rt.loopDefaultAgent
	}
	task, created, err := rt.store.TakeOverRequest(store.Task{
		RequestID: req.ID, AgentID: agentID,
		Title:     requestTitle(req),
		Goal:      requestGoal(req),
		Context:   clip(strings.TrimSpace(h.Context), maxTaskContext),
		ChannelID: req.ChannelID, Target: req.Target,
		Progress: handoffProgress(h) + delegationsNote(rt.delegationsFor(req.ID)),
	}, h.Why)
	if err != nil {
		return agent.HandoffResult{}, fmt.Errorf("take the request over: %w", err)
	}
	rt.log.Info("a request is now a task",
		zap.String("request", req.ID), zap.String("task", task.ID),
		zap.Bool("created", created), zap.String("why", h.Why))
	rt.kickTasks()

	ack := handoffAck(req)
	if req.ChannelID != "" && req.Target != "" {
		// A turn status, not an answer: the same words acknowledge every
		// request, so they must not be refused as a repeat of the last.
		if _, err := rt.deliverOnce(store.Delivery{Key: "ack:" + req.ID, ChannelID: req.ChannelID,
			Target: req.Target, Content: ack}, comms.ProducerStatus); err != nil {
			rt.log.Warn("could not acknowledge a request; its task carries on", zap.String("request", req.ID), zap.Error(err))
		}
	}
	return agent.HandoffResult{TaskID: task.ID, Ack: ack}, nil
}

// handoffAck is what the requester is told when their request becomes a task.
func handoffAck(req *agent.Request) string {
	return "On it — this takes more than a quick reply, so I'm working on it as a task. " +
		"I'll send the result here when it's done."
}

// requestTitle names a request in task lists: the requester's own words.
func requestTitle(req *agent.Request) string {
	words := strings.TrimSpace(req.Asked)
	if words == "" {
		words = strings.TrimSpace(req.Text)
	}
	return clip(firstLineOf(words), 120)
}

// requestGoal is the task's goal: the request as it was put to the turn, and
// where it came from.
func requestGoal(req *agent.Request) string {
	var b strings.Builder
	if asked := strings.TrimSpace(req.Asked); asked != "" {
		b.WriteString("Do what was asked:\n\n> " + strings.ReplaceAll(asked, "\n", "\n> ") + "\n\n")
	}
	b.WriteString("## The request, as it was given to the turn that started it\n\n")
	b.WriteString(strings.TrimSpace(req.Text))
	b.WriteString("\n\n## Who is waiting\n\n")
	b.WriteString("It came from " + orDefault(req.Where, "the operator") + ". ")
	if req.ChannelID != "" && req.Target != "" {
		b.WriteString("KARMAX delivers your final REPORT (and any FILE you name) back there when you finish.")
	} else {
		b.WriteString("KARMAX delivers your final REPORT (and any FILE you name) to the operator when you finish.")
	}
	return b.String()
}

// handoffProgress is where the task starts from: why the turn stopped and
// what it already did, so nothing is done twice.
func handoffProgress(h agent.Handoff) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Taken over from the turn that started it (%s).", orDefault(h.Why, "it could not finish"))
	if len(h.Done) > 0 {
		b.WriteString(" That turn had already done:\n")
		for _, d := range h.Done {
			b.WriteString("- " + d + "\n")
		}
		b.WriteString("Check what those left behind before redoing any of it.")
	} else {
		b.WriteString(" It had done nothing yet that KARMAX can see.")
	}
	return b.String()
}

// delegationsNote tells a task about delegations still working for its
// request: their results are added to its progress when they finish.
func delegationsNote(running []string) string {
	if len(running) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nStill running for this request, started by that turn (their results are added here when they finish — " +
		"do not start the same work again; if you need what they produce, end the round STATUS: working and it will be here):\n")
	for _, r := range running {
		b.WriteString("- " + r + "\n")
	}
	return b.String()
}

// lateTaskResult gives a task the rest of the turn it took over from: the
// session finished that turn after the hand-off. Appended to the task's
// progress, so its next round starts from it instead of redoing it.
func (rt *KarmaxRuntime) lateTaskResult(taskID string, t agent.HarnessTurn) {
	if rt.store == nil {
		return
	}
	rt.appendLate(taskID, "The turn this was taken over from finished after all", t)
}

// appendTaskProgress adds to what a task's next round reads, leaving its
// status, error and schedule as they are.
func (rt *KarmaxRuntime) appendTaskProgress(taskID, note string) error {
	task, found, err := rt.store.GetTask(taskID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no task %s", taskID)
	}
	return rt.store.UpdateTask(taskID, store.TaskUpdate{
		Progress: strings.TrimSpace(task.Progress + note), KeepLastError: true, KeepNextActionAt: true,
	})
}

// inFlight describes the requests tasks are working on for a chat ("" for
// the app), for the turns and loop calls about the same chat: one owner per
// request.
func (rt *KarmaxRuntime) inFlight(target string) []string {
	if rt.store == nil {
		return nil
	}
	tasks, err := rt.store.ListTasks("live", 50)
	if err != nil {
		return nil
	}
	norm := normalizeChat(target)
	var out []string
	for _, t := range tasks {
		if t.RequestID == "" || normalizeChat(t.Target) != norm {
			continue
		}
		out = append(out, fmt.Sprintf("%q — task %s, %s since %s", clip(t.Title, 140), t.ID, t.Status,
			t.CreatedAt.Local().Format("15:04")))
	}
	return out
}

// normalizeChat reduces a chat id to what two spellings of it share.
func normalizeChat(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "@:"); i >= 0 {
		s = s[:i]
	}
	return s
}

// deliverOnce sends text to a chat at most once under d.Key: claimed in the
// ledger before the send and marked after, and an attempt cut off between
// the two is looked for in the chat before it is sent again. sent is false
// when it had already gone.
func (rt *KarmaxRuntime) deliverOnce(d store.Delivery, producer string) (sent bool, err error) {
	return rt.deliverWith(d, func() error { return rt.sendFrom(producer, d.ChannelID, d.Target, d.Content) })
}

// deliverWith is deliverOnce with the send supplied.
func (rt *KarmaxRuntime) deliverWith(d store.Delivery, send func() error) (bool, error) {
	state, since, err := rt.store.BeginDelivery(d)
	if err != nil {
		// A ledger that cannot be read is not a reason to withhold the answer:
		// a possible repeat is better than certain silence.
		rt.log.Warn("delivery ledger unavailable; sending anyway", zap.String("key", d.Key), zap.Error(err))
		return true, send()
	}
	switch state {
	case store.DeliverySent:
		return false, nil
	case store.DeliveryPending:
		if rt.store.SentSince(d.Target, d.Content, since) {
			_ = rt.store.FinishDelivery(d.Key)
			return false, nil
		}
	}
	if err := send(); err != nil {
		if errors.Is(err, comms.ErrDuplicateSend) {
			// The channel already holds these words: it went out before.
			_ = rt.store.FinishDelivery(d.Key)
			return false, nil
		}
		return false, err
	}
	if err := rt.store.FinishDelivery(d.Key); err != nil {
		rt.log.Warn("could not record a delivery", zap.String("key", d.Key), zap.Error(err))
	}
	return true, nil
}

// sendFrom sends through comms, or the task runner's test seam.
func (rt *KarmaxRuntime) sendFrom(producer, channelID, target, text string) error {
	if rt.taskHooks.send != nil {
		return rt.taskHooks.send(channelID, target, text)
	}
	if rt.comms == nil {
		return fmt.Errorf("no comms")
	}
	return rt.comms.SendFrom(producer, channelID, target, text)
}

// sendFileTo sends a file through comms, or the task runner's test seam.
func (rt *KarmaxRuntime) sendFileTo(channelID, target, path string) error {
	if rt.taskHooks.sendFile != nil {
		return rt.taskHooks.sendFile(channelID, target, path)
	}
	if rt.comms == nil {
		return fmt.Errorf("no comms")
	}
	return rt.comms.SendFile("", channelID, target, path)
}

// notifyOnce puts a notice in the app at most once under key.
func (rt *KarmaxRuntime) notifyOnce(key, agentID, kind, title, body string) {
	_, _ = rt.deliverWith(store.Delivery{Key: key, Content: body}, func() error {
		builtin.PushAppNotification(rt.store, agentID, kind, title, body)
		return nil
	})
}

// resumeDelegations restarts the background delegations the daemon died
// during (builtin.ResumeDelegations), each for the agent that started it.
func (rt *KarmaxRuntime) resumeDelegations() {
	if rt.store == nil || rt.bus == nil {
		return
	}
	base := builtin.ClaudeCodeTool{Store: rt.store, Publish: rt.bus.Publish}
	if rt.cfg != nil {
		base.DataDir = rt.cfg.Karmax.DataDir
		base.EngineAPIURL, base.EngineBrowserToken = rt.apiBaseURL, rt.apiBrowserToken
	}
	if rt.memory != nil {
		base.MemoryMgr = rt.memory.For(rt.loopDefaultAgent, rt.loopNamespace())
	}
	if n := builtin.ResumeDelegations(base); n > 0 {
		rt.log.Info("resumed background delegations the daemon died during", zap.Int("count", n))
	}
}

// delegationToTask gives a finished delegation's result to the task that
// owns the request it was started for, instead of a turn that no longer
// knows who asked — and wakes that task. False when no live task owns it.
func (rt *KarmaxRuntime) delegationToTask(evt bus.Event) bool {
	if evt.Kind != bus.EventDelegationDone || rt.store == nil {
		return false
	}
	reqID, _ := evt.Payload["origin_request"].(string)
	task, found, err := rt.store.GetTaskForRequest(reqID)
	if err != nil || !found {
		return false
	}
	switch task.Status {
	case store.TaskOpen, store.TaskWorking, store.TaskBlocked:
	default:
		return false
	}
	status, _ := evt.Payload["status"].(string)
	out, _ := evt.Payload["output"].(string)
	if status == "failed" {
		out, _ = evt.Payload["error"].(string)
	}
	job, _ := evt.Payload["job_id"].(string)
	note := fmt.Sprintf("\n\n## A delegation started for this request has %s (job %s)\n\n%s\n\n"+
		"Use what it produced rather than redoing it.", orDefault(status, "finished"), job, clip(strings.TrimSpace(out), 4000))
	if err := rt.appendTaskProgress(task.ID, note); err != nil {
		rt.log.Warn("could not give a task its delegation's result", zap.String("task", task.ID), zap.Error(err))
		return false
	}
	if task.Status != store.TaskBlocked {
		now := time.Now()
		_ = rt.store.UpdateTask(task.ID, store.TaskUpdate{NextActionAt: &now, KeepLastError: true})
	}
	rt.log.Info("a delegation's result went to the task that owns its request",
		zap.String("task", task.ID), zap.String("job", job), zap.String("status", status))
	rt.kickTasks()
	return true
}

// delegationsFor describes the delegations still running for a request, so
// the task that takes it over waits for them instead of doing the work twice.
func (rt *KarmaxRuntime) delegationsFor(requestID string) []string {
	rows, err := rt.store.ListTasks(store.TaskDelegated, 50)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range rows {
		if r.RequestID == requestID {
			out = append(out, fmt.Sprintf("job %s (Claude Code session %s): %s", strings.TrimPrefix(r.ID, "delegation-"),
				r.SessionKey, clip(firstLineOf(r.Goal), 200)))
		}
	}
	return out
}

// chatRequest is the request a message in the app's chat makes, journalled
// before its turn starts; nil when nothing can take a request over.
func (rt *KarmaxRuntime) chatRequest(conversationID, message string) *agent.Request {
	if rt.requestOwner() == nil {
		return nil
	}
	req := agent.NewRequest("chat-"+uuid.New().String(), agent.RequestChat)
	req.Text, req.Asked, req.AgentID = message, message, rt.loopDefaultAgent
	req.Where = "the operator, in the KARMAX app (conversation " + conversationID + ")"
	if _, err := rt.store.StartAgentTurn(store.AgentTurn{
		ID: "turn-" + req.ID, AgentID: req.AgentID, EventID: req.ID, EventKind: req.Kind,
		EventJSON: req.JSON(), Attempt: 1, StartedAt: req.Started,
	}); err != nil {
		rt.log.Warn("could not journal a chat request; working it anyway", zap.Error(err))
	}
	return req
}

// finishRequestRow closes a journalled request's row; a row a task took over
// keeps saying so.
func (rt *KarmaxRuntime) finishRequestRow(req *agent.Request, err error) {
	if req == nil || rt.store == nil {
		return
	}
	status, msg := store.TurnOK, ""
	if err != nil {
		status, msg = store.TurnFailed, err.Error()
	}
	_ = rt.store.FinishAgentTurn(req.ID, status, msg)
}

// requestLateHandler is a session's OnLate for a request run outside an
// agent: the rest of the turn goes to the task that took the request over,
// or else to the operator, in the app — never to nobody.
func (rt *KarmaxRuntime) requestLateHandler(req *agent.Request) func(harness.Turn) {
	if req == nil {
		return nil
	}
	late := agent.LateForRequest(req, rt.requestOwner, func(t agent.HarnessTurn) {
		if text := strings.TrimSpace(t.Text); text != "" {
			rt.notifyOnce("late:"+req.ID, req.AgentID, "info", "Finished late: "+requestTitle(req), clip(text, 1500))
		}
	})
	return func(t harness.Turn) { late(agentTurn(t)) }
}

// toolNames describes what a turn ran, for the task that continues it.
func toolNames(calls []harness.ToolCall) []string {
	var out []string
	for _, tc := range calls {
		if tc.Failed {
			continue
		}
		line := tc.Name
		if tc.Command != "" {
			line = "ran `" + tc.Command + "`"
		}
		out = append(out, line)
	}
	return out
}

// recoverRequest continues a request whose turn the daemon died during and
// whose caller is gone with it (a workflow's ask, the app's chat): it can only
// go on as a task.
func (rt *KarmaxRuntime) recoverRequest(t store.AgentTurn) bool {
	req, err := agent.RequestFromJSON(t.EventJSON)
	if err != nil || req.ID == "" {
		rt.log.Warn("an interrupted request is unreadable", zap.String("request", t.EventID), zap.Error(err))
		return false
	}
	var done []string
	if sent, _ := rt.store.ListOutboundSince(req.Started.Truncate(time.Second), 20); len(sent) > 0 {
		for _, m := range sent {
			if comms.IsAnswerProducer(m.SenderID) {
				done = append(done, fmt.Sprintf("sent to %s: %q", m.ChannelID, clip(m.Content, 120)))
			}
		}
	}
	res, err := rt.handOff(agent.Handoff{Request: req, Done: done,
		Why: "KARMAX restarted while the turn was working on it"})
	if err != nil {
		rt.log.Error("could not continue an interrupted request as a task", zap.String("request", req.ID), zap.Error(err))
		return false
	}
	if req.ChannelID == "" || req.Target == "" {
		// No chat to acknowledge in, and nobody left waiting on the call.
		rt.notifyOnce("ack:"+req.ID, req.AgentID, "info", "Still on it: "+requestTitle(req), res.Ack)
	}
	return true
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
