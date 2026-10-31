package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/policy"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// Work that takes longer than a turn, the whole way through.
//
// The fakes are at the edges only: a scripted Claude Code CLI (the harness),
// and the channel the answers go out on. Between them everything is real —
// the router and turn journal, the agent's turn and the harness brain, the
// supervisor timing the turn out, the hand-off to a task, the task runner and
// its session, and the delivery ledger.

const (
	opChat   = "100000000000001@lid"
	teamChat = "120363000000000001@g.us"
	waID     = "whatsapp-main"
)

// longWorkCLI is a Claude Code that starts the work (a tool call) and then
// takes longer than any turn allows — except when it is asked to continue a
// task, which it finishes, naming the file it made.
func longWorkCLI(t *testing.T, deck string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude.sh")
	body := "#!/bin/bash\n" +
		"while IFS= read -r line; do\n" +
		"  case \"$line\" in\n" +
		"    *'Continue this task'*)\n" +
		"      echo '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"t2\",\"name\":\"Bash\",\"input\":{\"command\":\"typst compile deck.typ\"}}]}}'\n" +
		"      echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"Built the deck from the repos.\\nREPORT: Here is the onboarding deck for Northwind: six pages, no filler.\\nFILE: " + deck + "\\nSTATUS: done\",\"usage\":{}}'\n" +
		"      ;;\n" +
		"    *)\n" +
		"      echo '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Bash\",\"input\":{\"command\":\"git log --oneline\"}}]}}'\n" +
		"      sleep 30\n" +
		"      ;;\n" +
		"  esac\n" +
		"done\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// sent is one message or file that went out.
type sent struct{ channel, target, text, file string }

type longWorkFlow struct {
	rt     *KarmaxRuntime
	a      *agent.Agent
	db     *store.Store
	deck   string
	cancel context.CancelFunc

	mu       sync.Mutex
	out      []sent
	fallback int
}

// record notes what went out and, as comms does after every send, stores it
// among the chat's messages — which is what a delivery cut off between
// sending and recording is found by.
func (f *longWorkFlow) record(s sent) {
	f.mu.Lock()
	f.out = append(f.out, s)
	f.mu.Unlock()
	content := s.text
	if s.file != "" {
		content = "[file] " + filepath.Base(s.file)
	}
	_ = f.db.SaveChannelMessage(store.StoredChannelMessage{ID: "out-" + time.Now().Format(time.RFC3339Nano) + content,
		ChannelID: s.target, ChannelType: "whatsapp", Direction: "outbound", Content: content})
}

func (f *longWorkFlow) sentTo(target string) []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var got []sent
	for _, s := range f.out {
		if s.target == target {
			got = append(got, s)
		}
	}
	return got
}

// waitFor waits until target has received n things, and returns them.
func (f *longWorkFlow) waitFor(t *testing.T, target string, n int) []sent {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.sentTo(target); len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s received %d of %d: %+v", target, len(f.sentTo(target)), n, f.sentTo(target))
	return nil
}

// fallbackBrain is the metered API brain: a request that ran out of time on
// the harness must never be handed to it.
type fallbackBrain struct{ f *longWorkFlow }

func (b fallbackBrain) ProcessMessageWithheld(context.Context, string, []tools.Tool, map[string]bool) (string, []karmahelper.ToolCallRecord, error) {
	b.f.mu.Lock()
	b.f.fallback++
	b.f.mu.Unlock()
	return "Sorry, I can't make a deck.", nil, nil
}
func (fallbackBrain) SetTurnContext(string)             {}
func (fallbackBrain) NeedsCompaction() bool             { return false }
func (fallbackBrain) GetHistory() *models.AIChatHistory { return &models.AIChatHistory{} }
func (fallbackBrain) SetHistory(models.AIChatHistory)   {}
func (fallbackBrain) GetTotalTokens() int64             { return 0 }
func (fallbackBrain) GetKeepRecent() int                { return 10 }
func (fallbackBrain) ResetTokenCount()                  {}

// newLongWorkFlow is a daemon on the database at dbPath ("" for a new one),
// whose work produces a deck beside it.
func newLongWorkFlow(t *testing.T, dbPath string) *longWorkFlow {
	t.Helper()
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), "t.db")
	}
	return newLongWorkFlowDeck(t, dbPath, filepath.Join(filepath.Dir(dbPath), "northwind-deck.pdf"))
}

func newLongWorkFlowDeck(t *testing.T, dbPath, deck string) *longWorkFlow {
	t.Helper()
	dir := t.TempDir()
	db, err := store.New(dbPath, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := os.WriteFile(deck, []byte("%PDF-1.7 fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := bus.NewLog(db, "test", zap.NewNop())
	factory := memory.NewFactory(filepath.Join(dir, "memory"), db, zap.NewNop())
	reg := agent.NewRegistry(b, db, zap.NewNop())
	a, err := reg.Register(agent.AgentDef{ID: "nexus", Name: "Test agent", Provider: "none", Model: "none"},
		factory.For("nexus", ""), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &longWorkFlow{a: a, db: db, deck: deck}

	sup := harness.New(harness.Config{
		Binary: longWorkCLI(t, deck), WorkdirRoot: filepath.Join(dir, "sessions"), MaxLive: 4, Env: os.Environ(),
		Policies: map[string]harness.Policy{
			"orchestrator": {TurnTimeout: 1500 * time.Millisecond, Idle: time.Minute},
			"task":         {TurnTimeout: 20 * time.Second, Idle: time.Minute},
		},
	}, harnessStore{db}, harness.NewBreaker(0.95, nil), harnessLog{zap.NewNop()}, nil)
	t.Cleanup(sup.Shutdown)

	rt := &KarmaxRuntime{store: db, log: zap.NewNop(), bus: b, agents: reg, memory: factory,
		harness: sup, loopDefaultAgent: "nexus", policy: policy.Default(), startedAt: time.Now()}
	rt.taskHooks.send = func(channel, target, text string) error {
		f.record(sent{channel: channel, target: target, text: text})
		return nil
	}
	rt.taskHooks.sendFile = func(channel, target, path string) error {
		f.record(sent{channel: channel, target: target, file: path})
		return nil
	}
	f.rt = rt

	a.SetOperatorChats([]string{opChat})
	a.SetCommsSend(func(channel, target, text string) error {
		f.record(sent{channel: channel, target: target, text: text})
		return nil
	})
	a.SetHarnessBrain(agent.NewHarnessBrain(harnessSenderFor{sup: sup, prompts: map[string]string{}},
		"agent:nexus", "orchestrator", fallbackBrain{f}))
	a.SetTaskOwner(rt.requestOwner())

	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	t.Cleanup(cancel)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop() })
	rt.startTaskRunner(ctx)
	return f
}

func operatorMessage(text string) bus.Event {
	return bus.NewEvent(bus.EventCommsMessage, "nexus", map[string]any{
		"karmax_channel_id": waID, "channel_id": opChat, "content": text, "chat_name": "Operator",
		"sender_is_operator": true,
	})
}

// The incident, generally: the operator asks for something that takes
// minutes. The turn starts it, runs out of time — and instead of failing,
// falling back to a lighter model, or losing what it did, the request becomes
// a task. The operator hears "on it" at once and the result when it is done,
// each exactly once, in that order.
func TestARequestThatOutrunsItsTurnContinuesAsATaskAndDelivers(t *testing.T) {
	f := newLongWorkFlow(t, "")
	evt := operatorMessage("make an onboarding deck for Northwind from the work we did, no filler")
	if err := f.rt.routeToAgent(evt); err != nil {
		t.Fatal(err)
	}

	got := f.waitFor(t, opChat, 3)
	if !strings.Contains(got[0].text, "On it") {
		t.Errorf("first the acknowledgement; got %+v", got[0])
	}
	if !strings.Contains(got[1].text, "onboarding deck for Northwind") {
		t.Errorf("then the result; got %+v", got[1])
	}
	if got[2].file != f.deck {
		t.Errorf("then the file it made; got %+v", got[2])
	}
	time.Sleep(300 * time.Millisecond)
	if more := f.sentTo(opChat); len(more) != 3 {
		t.Errorf("the chat got %d messages, want exactly the three: %+v", len(more), more)
	}
	if f.fallback != 0 {
		t.Errorf("the request was retried on the lighter fallback %d time(s)", f.fallback)
	}

	task, found, err := f.db.GetTaskForRequest(evt.ID)
	if err != nil || !found {
		t.Fatalf("no task owns the request: %v", err)
	}
	if task.Status != store.TaskDone || task.Target != opChat || task.ChannelID != waID {
		t.Errorf("task = %+v", task)
	}
	if !strings.Contains(task.Goal, "onboarding deck for Northwind") {
		t.Errorf("the task does not carry the request: %q", task.Goal)
	}
	if !strings.Contains(task.Progress+task.Reported, "deck") {
		t.Errorf("the task kept nothing of its work: %+v", task)
	}
	if st := turnStatusFor(t, f.db, evt.ID); st != store.TurnHandedOff {
		t.Errorf("the turn row says %q, want handed_off", st)
	}
	if owed, _ := f.db.OwedDeliveries("task:"); len(owed) != 0 {
		t.Errorf("results still owed after delivery: %+v", owed)
	}
}

// The same through a workflow's ask(): the operator addressed KARMAX in a
// group the workflow watches. The group gets the acknowledgement and the
// result from the task — and nothing else from the run that asked: no
// lighter-model retry, no fallback message.
func TestAWorkflowAskThatOutrunsItsTurnContinuesAsATask(t *testing.T) {
	f := newLongWorkFlow(t, "")
	run := &loopRunState{}
	k := &loopKit{loopName: "chat-monitor", agentID: "nexus", rt: f.rt, run: run,
		trigger: loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: map[string]any{
			"karmax_channel_id": waID, "channel_id": teamChat, "chat_name": "Lakeside team",
			"content": "@karmax make the Northwind onboarding deck", "sender_is_operator": true,
		}}}

	out, err := k.AskWithTools(context.Background(), "The operator directly instructed you in the group: make the Northwind onboarding deck.", nil)
	if err != nil {
		t.Fatalf("ask = %v; a request that outran its turn is not a failure", err)
	}
	if !strings.Contains(out, "On it") {
		t.Errorf("ask answered %q, want the acknowledgement", out)
	}
	if err := run.refusal("gateway"); err == nil {
		t.Error("the run still thinks it owns a request a task took over")
	}
	if _, err := k.Gateway(context.Background(), "is this worth answering?"); err == nil || !strings.Contains(err.Error(), "task") {
		t.Errorf("the gateway answered a request a task owns: %v", err)
	}

	got := f.waitFor(t, teamChat, 3)
	if !strings.Contains(got[0].text, "On it") || !strings.Contains(got[1].text, "onboarding deck") || got[2].file != f.deck {
		t.Errorf("the group got %+v, want the acknowledgement, the result and the file", got)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(f.sentTo(teamChat)); n != 3 {
		t.Errorf("the group got %d messages, want exactly three", n)
	}
	if f.fallback != 0 {
		t.Errorf("the ask was retried on the fallback %d time(s)", f.fallback)
	}
}

// Someone other than the operator asking through a workflow is a request
// too — but its result goes to the operator, who decides what reaches that
// chat, never straight into it.
func TestAThirdPartysRequestIsReportedToTheOperatorNotTheirChat(t *testing.T) {
	f := newLongWorkFlow(t, "")
	k := &loopKit{loopName: "chat-monitor", agentID: "nexus", rt: f.rt, run: &loopRunState{},
		trigger: loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: map[string]any{
			"karmax_channel_id": waID, "channel_id": teamChat, "chat_name": "Lakeside team",
			"content": "can you send me the deck?", "sender_label": "Dana Reyes",
		}}}
	req := k.askRequest("prompt")
	if req == nil || req.ChannelID != "" || req.Target != "" {
		t.Fatalf("a third party's request is owed to their chat: %+v", req)
	}
	if !strings.Contains(req.Where, "Dana Reyes") || !strings.Contains(req.Where, "operator") {
		t.Errorf("where = %q", req.Where)
	}
}

// A run whose request a task owns says nothing more in that chat: the send
// that would have been an "away" note is refused. Other chats are untouched.
func TestARunDoesNotSpeakForARequestATaskOwns(t *testing.T) {
	run := &loopRunState{}
	if err := run.refusal("sending into this chat"); err != nil {
		t.Fatalf("a run that settled nothing refused: %v", err)
	}
	run.handedTo("req-ask-1")
	if err := run.refusal("sending into this chat"); err == nil {
		t.Fatal("a run kept speaking for a request a task owns")
	}
	if !sendsTo(map[string]any{"to": teamChat, "text": "He's away right now"}, teamChat) {
		t.Error("a send to the run's chat was not recognised")
	}
	if !sendsTo(map[string]any{"to": "120363000000000001"}, teamChat) {
		t.Error("the same chat spelled without its server was not recognised")
	}
	if sendsTo(map[string]any{"to": opChat}, teamChat) {
		t.Error("a send to another chat was taken for the run's chat")
	}

	failed := &loopRunState{}
	failed.gaveUp("harness turn timed out after 3m0s")
	if err := failed.refusal("harness"); err == nil || !strings.Contains(err.Error(), "lighter model") {
		t.Errorf("after the full agent gave up, refusal = %v", err)
	}
}

// A later pass over the same chat is told what tasks already own there, so it
// neither answers them again nor asks for what they need.
func TestALaterPassIsToldWhatIsAlreadyOwned(t *testing.T) {
	f := newLongWorkFlow(t, "")
	if _, _, err := f.db.TakeOverRequest(store.Task{RequestID: "ask-7", Title: "Northwind onboarding deck",
		Goal: "make it", ChannelID: waID, Target: teamChat}, "out of time"); err != nil {
		t.Fatal(err)
	}
	k := &loopKit{loopName: "chat-monitor", rt: f.rt, trigger: loopkit.Trigger{Kind: loopkit.TriggerEvent,
		Payload: map[string]any{"channel_id": teamChat, "content": "any update on the deck?"}}}
	p := k.withIdentity("Answer the latest message.")
	if !strings.Contains(p, "Northwind onboarding deck") || !strings.Contains(p, "Do not answer them") {
		t.Errorf("the prompt does not say what is owned:\n%s", p)
	}
	other := &loopKit{loopName: "chat-monitor", rt: f.rt, trigger: loopkit.Trigger{Kind: loopkit.TriggerEvent,
		Payload: map[string]any{"channel_id": opChat}}}
	if p := other.withIdentity("x"); strings.Contains(p, "Northwind") {
		t.Errorf("another chat was told about this chat's task:\n%s", p)
	}
}

// A delegation the turn started finishes after the request became a task:
// its result goes to that task — waking it — not to a turn that no longer
// knows who asked. With no task owning the request, it goes to the agent.
func TestADelegationsResultGoesToTheTaskThatOwnsItsRequest(t *testing.T) {
	f := newLongWorkFlow(t, "")
	task, _, err := f.db.TakeOverRequest(store.Task{RequestID: "ev-9", Goal: "make the deck",
		ChannelID: waID, Target: opChat, Status: store.TaskBlocked}, "out of time")
	if err != nil {
		t.Fatal(err)
	}
	done := bus.NewEvent(bus.EventDelegationDone, "nexus", map[string]any{
		"job_id": "job-1", "status": "completed", "output": "Wrote /srv/out/northwind.pdf",
		"origin_request": "ev-9", "origin_target": opChat,
	})
	if err := f.rt.routeToAgent(done); err != nil {
		t.Fatal(err)
	}
	got, _, _ := f.db.GetTask(task.ID)
	if !strings.Contains(got.Progress, "/srv/out/northwind.pdf") {
		t.Errorf("the task did not get its delegation's result: %q", got.Progress)
	}
	if _, found, _ := f.db.AgentTurnByEvent(done.ID); found {
		t.Error("a turn was opened for a result its task already has")
	}

	other := bus.NewEvent(bus.EventDelegationDone, "nexus", map[string]any{
		"job_id": "job-2", "status": "completed", "output": "done", "origin_request": "ev-unowned",
	})
	if f.rt.delegationToTask(other) {
		t.Error("a result no task owns was kept from the agent")
	}
}

// sendRecorder is a sending tool that records what it was asked to send.
type sendRecorder struct {
	mu   sync.Mutex
	sent []string
}

func (s *sendRecorder) Manifest() tools.ToolManifest {
	return tools.ToolManifest{Name: "whatsapp_send_message"}
}
func (s *sendRecorder) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text, _ := in["text"].(string)
	s.sent = append(s.sent, text)
	return tools.SuccessResult(map[string]any{"sent": true}), nil
}

// The operator's own message is never answered as if they were someone else
// — whichever route of whichever workflow composed the reply — and a run
// whose request a task owns says nothing more in that chat. Real answers to
// the operator, and messages to other people, still go.
func TestTheOperatorsOwnMessageIsNotAnsweredAsIfTheyWereAway(t *testing.T) {
	reg := tools.NewRegistry()
	send := &sendRecorder{}
	reg.Register(send)
	rt := &KarmaxRuntime{log: zap.NewNop(), tools: reg}
	rt.taskHooks.decide = func(_ context.Context, state any, _ loopkit.Questions) (*loopkit.Decision, error) {
		reply, _ := state.(map[string]any)["reply"].(string)
		yes := 0.05
		if strings.Contains(reply, "away") {
			yes = 0.95
		}
		return &loopkit.Decision{Answers: map[string]loopkit.Answer{"treats_operator_as_other": {Noul: yes}}}, nil
	}
	w := &wasmKit{rt: rt, loop: "chat-monitor"}
	w.run.Store(&loopRunState{})
	w.setTrigger(loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: map[string]any{
		"channel_id": teamChat, "content": "@karmax make the Northwind deck", "sender_is_operator": true,
	}})
	ctx := context.Background()

	if _, err := w.Tool(ctx, "whatsapp_send_message", map[string]any{"to": teamChat,
		"text": "Hi, this is KARMAX — he's away from his phone, I've let him know."}); err == nil {
		t.Error("an away note went to the operator who had just asked")
	}
	if _, err := w.Tool(ctx, "whatsapp_send_message", map[string]any{"to": teamChat,
		"text": "On it — the deck will be here in a few minutes."}); err != nil {
		t.Errorf("a real answer to the operator was refused: %v", err)
	}

	w.run.Load().handedTo("req-ask-9")
	if _, err := w.Tool(ctx, "whatsapp_send_message", map[string]any{"to": teamChat, "text": "Here's a quick version."}); err == nil {
		t.Error("the run spoke for a request a task owns")
	}
	if _, err := w.Tool(ctx, "whatsapp_send_message", map[string]any{"to": opChat, "text": "FYI"}); err != nil {
		t.Errorf("a message to another chat was refused: %v", err)
	}
	if len(send.sent) != 2 {
		t.Errorf("sent %v, want only the real answer and the other chat's message", send.sent)
	}

	// A third party's message: the guard does not ask, the note is theirs.
	asked := false
	rt.taskHooks.decide = func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error) {
		asked = true
		return &loopkit.Decision{}, nil
	}
	w.run.Store(&loopRunState{})
	w.setTrigger(loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: map[string]any{"channel_id": teamChat, "content": "hi"}})
	if _, err := w.Tool(ctx, "whatsapp_send_message", map[string]any{"to": teamChat, "text": "He's away, I'll let him know."}); err != nil || asked {
		t.Errorf("a note to a third party was checked or refused: err=%v asked=%v", err, asked)
	}
}

func turnStatusFor(t *testing.T, db *store.Store, eventID string) string {
	t.Helper()
	row, found, err := db.AgentTurnByEvent(eventID)
	if err != nil || !found {
		t.Fatalf("no journal row for %s: %v", eventID, err)
	}
	return row.Status
}
