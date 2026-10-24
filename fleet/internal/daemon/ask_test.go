package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// askOn starts an agent's ask in the background and returns once it is
// blocked; the reply arrives on the channel.
func askOn(t *testing.T, f *Fleet, ctx context.Context, agent, task, q string, timeout time.Duration, at time.Time) chan askResult {
	t.Helper()
	done := make(chan askResult, 1)
	go func() {
		rep, err := f.Ask(ctx, agent, q, timeout, at)
		done <- askResult{rep, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.askMu.Lock()
		_, blocked := f.asks[task]
		f.askMu.Unlock()
		if blocked && questionFiled(f, task) {
			return done
		}
		select {
		case r := <-done:
			t.Fatalf("ask ended at once: %+v, %v", r.rep, r.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the ask never blocked")
		}
		time.Sleep(time.Millisecond)
	}
}

// questionFiled reports whether the task has an unanswered question on the ledger.
func questionFiled(f *Fleet, task string) bool {
	qs, _ := f.db.Unanswered()
	for _, q := range qs {
		if q.Task == task {
			return true
		}
	}
	return false
}

// eventually polls until cond holds, for up to five seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("never happened: %s", what)
		}
	}
}

type askResult struct {
	rep AskReply
	err error
}

func within(t *testing.T, ch chan askResult) askResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("the ask did not return")
		return askResult{}
	}
}

func runT1(t *testing.T, f *Fleet, from string) {
	t.Helper()
	if _, err := f.Run(context.Background(), RunRequest{Brief: "fix the login redirect\nsecond line", Agent: "agent-01", From: from, Task: "T1"}, t0); err != nil {
		t.Fatal(err)
	}
}

func TestAnAnswerReachesTheBlockedAskAsItsOutputAndNotAsAMessage(t *testing.T) {
	f, boxes := newPool(t)
	ctx := context.Background()
	runT1(t, f, "operator")
	done := askOn(t, f, ctx, "agent-01", "T1", "which branch do I base on?", time.Minute, t0)

	in, _ := f.db.Inbox(0)
	if len(in) != 1 || in[0].Kind != ledger.KindQuestion || in[0].Task != "T1" || in[0].Text != "which branch do I base on?" {
		t.Fatalf("operator's inbox = %+v", in)
	}
	blocked, err := f.Answer(ctx, "T1", "operator", "origin/main", t0.Add(time.Minute))
	if err != nil || !blocked {
		t.Fatalf("answer = %v, %v", blocked, err)
	}
	r := within(t, done)
	if r.err != nil || r.rep.Answer != "origin/main" || r.rep.TimedOut {
		t.Fatalf("ask = %+v, %v", r.rep, r.err)
	}
	if calls := boxes["agent-01"].snapshotCalls(); len(calls) != 1 { // the brief, and nothing after
		t.Fatalf("the agent was sent %q: the answer is the ask's output, not a new turn", calls)
	}
	th, _ := f.db.TaskThread("T1")
	if last := th[len(th)-1]; last.Kind != ledger.KindAnswer || last.To != "agent-01" || last.Text != "origin/main" {
		t.Fatalf("thread ends %+v", last)
	}
	if ok, _ := f.db.Owes("T1"); !ok {
		t.Error("an answered agent owes the next word")
	}
}

func TestAnAskThatGetsNoAnswerTimesOutAndLeavesTheQuestionFiled(t *testing.T) {
	f, _ := newPool(t)
	runT1(t, f, "operator")
	rep, err := f.Ask(context.Background(), "agent-01", "anyone?", 30*time.Millisecond, t0)
	if err != nil || !rep.TimedOut || rep.Answer != "" {
		t.Fatalf("ask = %+v, %v", rep, err)
	}
	if in, _ := f.db.Inbox(0); len(in) != 1 || in[0].Kind != ledger.KindQuestion {
		t.Fatalf("inbox = %+v", in)
	}
	f.askMu.Lock()
	n := len(f.asks)
	f.askMu.Unlock()
	if n != 0 {
		t.Fatal("a finished ask is still registered")
	}
}

func TestAnAnswerWithNothingBlockedIsDeliveredAsAFollowup(t *testing.T) {
	f, boxes := newPool(t)
	runT1(t, f, "operator")
	blocked, err := f.Answer(context.Background(), "T1", "operator", "use main", t0.Add(time.Minute))
	if err != nil || blocked {
		t.Fatalf("answer = %v, %v", blocked, err)
	}
	calls := boxes["agent-01"].snapshotCalls()
	if len(calls) != 2 || !strings.Contains(calls[1], "Task T1 follow-up from operator. use main") {
		t.Fatalf("calls = %q", calls)
	}
}

func TestAFollowupOnATaskWithAPendingQuestionAnswersIt(t *testing.T) {
	f, boxes := newPool(t)
	ctx := context.Background()
	runT1(t, f, "operator")
	done := askOn(t, f, ctx, "agent-01", "T1", "which?", time.Minute, t0)
	if err := f.Followup(ctx, "T1", "operator", "the first", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if r := within(t, done); r.rep.Answer != "the first" {
		t.Fatalf("ask = %+v", r.rep)
	}
	if len(boxes["agent-01"].snapshotCalls()) != 1 {
		t.Fatal("the follow-up was also sent as a message")
	}
	// The ask is over: the next follow-up is an ordinary one.
	if blocked, err := f.Answer(ctx, "T1", "operator", "and then", t0.Add(2*time.Minute)); err != nil || blocked {
		t.Fatalf("second answer = %v, %v", blocked, err)
	}
}

func TestAnAskNeedsATaskAndAnAgent(t *testing.T) {
	f, _ := newPool(t)
	ctx := context.Background()
	if _, err := f.Ask(ctx, "agent-01", "q", time.Second, t0); err == nil || !strings.Contains(err.Error(), "no task") {
		t.Fatalf("no task: %v", err)
	}
	if _, err := f.Ask(ctx, "operator", "q", time.Second, t0); err == nil {
		t.Fatal("a person asked")
	}
}

func TestAQuestionToTheOrchestratorIsRelayedWithHowToAnswerIt(t *testing.T) {
	f, boxes := newPool(t)
	ctx := context.Background()
	runT1(t, f, "karmax")
	done := askOn(t, f, ctx, "agent-01", "T1", "rebase or merge?", time.Minute, t0)
	eventually(t, "the question reaching the orchestrator", func() bool { return len(boxes["karmax"].snapshotCalls()) > 0 })
	calls := boxes["karmax"].snapshotCalls()
	if len(calls) != 1 || !strings.Contains(calls[0], "rebase or merge?") || !strings.Contains(calls[0], `fleetctl answer T1 "<text>"`) {
		t.Fatalf("orchestrator was sent %q", calls)
	}
	if _, err := f.Answer(ctx, "T1", "karmax", "merge", t0); err != nil {
		t.Fatal(err)
	}
	if r := within(t, done); r.rep.Answer != "merge" {
		t.Fatalf("ask = %+v", r.rep)
	}
}

func TestAnUnansweredQuestionGoesToTheOrchestratorAfterTheWindowThenOneAlert(t *testing.T) {
	f, boxes := newPool(t)
	o := &outbox{}
	f.notify = o
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runT1(t, f, "review-1")
	done := askOn(t, f, ctx, "agent-01", "T1", "is the schema frozen?", time.Hour, t0)
	_ = done
	window := f.cfg.Thresholds.QuestionEscalate
	if window != 10*time.Minute {
		t.Fatalf("default window = %s", window)
	}

	f.escalate(ctx, t0.Add(window-time.Second))
	if len(boxes["karmax"].snapshotCalls()) != 0 {
		t.Fatal("escalated before the window")
	}
	f.escalate(ctx, t0.Add(window))
	calls := boxes["karmax"].snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("orchestrator was sent %q", calls)
	}
	for _, want := range []string{"Task T1", "agent-01", "is the schema frozen?", "fix the login redirect", "second line", "fleetctl answer T1"} {
		if !strings.Contains(calls[0], want) {
			t.Errorf("escalation lacks %q:\n%s", want, calls[0])
		}
	}
	if th, _ := f.db.TaskThread("T1"); len(th) != 2 {
		t.Fatalf("the escalation was filed under the task: %+v", th)
	}
	// Once only, however often it looks.
	f.escalate(ctx, t0.Add(window+time.Minute))
	if len(boxes["karmax"].snapshotCalls()) != 1 || len(o.alerts) != 0 {
		t.Fatalf("calls %q alerts %q", boxes["karmax"].snapshotCalls(), o.alerts)
	}
	// A further window with no answer: one alert.
	f.escalate(ctx, t0.Add(2*window))
	f.escalate(ctx, t0.Add(3*window))
	f.escalate(ctx, t0.Add(4*window))
	if len(o.alerts) != 1 || !strings.Contains(o.alerts[0], "agent-01") || !strings.Contains(o.alerts[0], "T1") {
		t.Fatalf("alerts = %q", o.alerts)
	}
	if len(boxes["karmax"].snapshotCalls()) != 1 {
		t.Fatal("escalated twice")
	}
}

func TestTheOrchestratorsAnswerStopsTheEscalation(t *testing.T) {
	f, _ := newPool(t)
	o := &outbox{}
	f.notify = o
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runT1(t, f, "operator")
	done := askOn(t, f, ctx, "agent-01", "T1", "which?", time.Hour, t0)
	window := f.cfg.Thresholds.QuestionEscalate
	f.escalate(ctx, t0.Add(window))
	if _, err := f.Answer(ctx, "T1", "karmax", "this one", t0.Add(window)); err != nil {
		t.Fatal(err)
	}
	if r := within(t, done); r.rep.Answer != "this one" {
		t.Fatalf("ask = %+v", r.rep)
	}
	f.escalate(ctx, t0.Add(3*window))
	if len(o.alerts) != 0 {
		t.Fatalf("alerts = %q after it was answered", o.alerts)
	}
}

func TestQuestionsToAPeerOrAPersonAreNotEscalated(t *testing.T) {
	f, boxes := newPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for task, c := range map[string]struct{ agent, from string }{"T1": {"agent-01", "karmax"}, "T2": {"agent-02", ram}} {
		if _, err := f.Run(ctx, RunRequest{Brief: "work", Agent: c.agent, From: c.from, Task: task}, t0); err != nil {
			t.Fatal(err)
		}
		askOn(t, f, ctx, c.agent, task, "q "+task, time.Hour, t0)
	}
	before := len(boxes["karmax"].snapshotCalls())
	f.escalate(ctx, t0.Add(5*time.Hour))
	if got := len(boxes["karmax"].snapshotCalls()); got != before {
		t.Fatalf("the orchestrator was sent %d more messages", got-before)
	}
}

func TestMessageKindsAreFiledAndShownByWaitAndResult(t *testing.T) {
	f, _ := newPool(t)
	ctx := context.Background()
	runT1(t, f, "operator")
	if err := f.TellKind(ctx, "agent-01", "operator", "halfway", ledger.KindProgress, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.TellKind(ctx, "agent-01", "operator", "x", "gossip", t0); err == nil {
		t.Fatal("a made-up kind went")
	}
	w, err := f.Wait(ctx, "", "T1", 0)
	if err != nil || w.Message.Kind != ledger.KindProgress {
		t.Fatalf("wait = %+v, %v", w, err)
	}
	if err := f.Tell(ctx, "agent-01", "operator", "all done", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	res, _ := f.Result("T1")
	var kinds []string
	for _, m := range res.Reports {
		kinds = append(kinds, m.Kind)
	}
	if strings.Join(kinds, ",") != "report,progress,report" {
		t.Fatalf("kinds = %v", kinds)
	}
}

func TestATellToAnotherAgentIsNotAReportOnTheSendersTask(t *testing.T) {
	f, boxes := newPool(t)
	ctx := context.Background()
	runT1(t, f, "operator")
	if err := f.Tell(ctx, "agent-01", "agent-02", "can you look at the schema?", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if calls := boxes["agent-02"].snapshotCalls(); len(calls) != 1 {
		t.Fatalf("agent-02 was sent %q", calls)
	}
	if th, _ := f.db.TaskThread("T1"); len(th) != 1 {
		t.Fatalf("the peer tell is filed under the task: %+v", th)
	}
	if owes, _ := f.db.Owes("T1"); !owes {
		t.Error("a peer tell cleared the owes-a-reply flag")
	}
	wctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if w, err := f.Wait(wctx, "", "T1", 0); err != nil || w.Reason != "timeout" {
		t.Fatalf("a peer tell woke the assigner's wait: %+v, %v", w, err)
	}
	// A word to its assigner, even an agent, is still the report.
	if _, err := f.Run(ctx, RunRequest{Brief: "sub", Agent: "agent-03", From: "agent-01", Task: "T9"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := f.Tell(ctx, "agent-03", "agent-01", "done", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if th, _ := f.db.TaskThread("T9"); len(th) != 2 {
		t.Fatalf("a report to an agent assigner is not filed: %+v", th)
	}
}

func TestASessionIsAnInboxLikeAPerson(t *testing.T) {
	f, boxes := newPool(t)
	ctx := context.Background()
	runT1(t, f, "review-1")
	if err := f.Tell(ctx, "agent-01", "review-1", "found it", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.TellKind(ctx, "agent-01", "review-1", "half way", ledger.KindProgress, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for n, b := range boxes {
		if n != "agent-01" && len(b.snapshotCalls()) > 0 {
			t.Fatalf("%s was sent %q: a session's messages are filed, not relayed", n, b.snapshotCalls())
		}
	}
	in, err := f.InboxOf("review-1", 0)
	if err != nil || len(in) != 2 || in[0].Text != "found it" || in[1].Kind != ledger.KindProgress {
		t.Fatalf("inbox = %+v, %v", in, err)
	}
	if op, _ := f.Inbox(0); len(op) != 0 {
		t.Fatalf("operator's inbox = %+v", op)
	}
	// Its follow-ups are its own, though it never ran anything under another name.
	if err := f.Followup(ctx, "T1", "review-1", "and one more", t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.Tell(ctx, "agent-01", "nobody-1", "x", t0); err == nil {
		t.Fatal("a name nobody uses took a message")
	}
}

func TestWaitNewHandsOverEachMessageOnceAndKeepsACursorPerReader(t *testing.T) {
	f, _ := newPool(t)
	ctx := context.Background()
	runT1(t, f, "review-1")
	_ = f.TellKind(ctx, "agent-01", "review-1", "one", ledger.KindProgress, t0.Add(time.Minute))
	_ = f.Tell(ctx, "agent-01", "review-1", "two", t0.Add(2*time.Minute))

	next := func(reader string) string {
		t.Helper()
		wctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		w, err := f.WaitNew(wctx, "", "T1", reader)
		if err != nil {
			t.Fatal(err)
		}
		if w.Reason != "message" {
			return w.Reason
		}
		return w.Message.Text
	}
	if a, b, c := next("review-1"), next("review-1"), next("review-1"); a != "one" || b != "two" || c != "timeout" {
		t.Fatalf("review-1 got %q, %q, %q", a, b, c)
	}
	if got := next("review-2"); got != "one" {
		t.Fatalf("another reader's first = %q", got)
	}
	_ = f.Tell(ctx, "agent-01", "review-1", "three", t0.Add(3*time.Minute))
	if got := next("review-1"); got != "three" {
		t.Fatalf("after a new report = %q", got)
	}
}

func TestSessionSubnamesBelongToTheirPersonAndCountAgainstTheirLimit(t *testing.T) {
	f, _ := newPool(t)
	ctx := context.Background()
	for i, id := range []string{"T1", "T2"} {
		if _, err := f.Run(ctx, RunRequest{Brief: "work", From: ram + "/s" + string(rune('a'+i)), Task: id, Limit: 2}, t0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Run(ctx, RunRequest{Brief: "more", From: ram + "/sc", Task: "T3", Limit: 2}, t0); err == nil || !strings.Contains(err.Error(), "holds 2 open tasks") {
		t.Fatalf("third run across sessions: %v", err)
	}
	if open, _ := f.db.TasksBy(true, "", ram, 0); len(open) != 2 {
		t.Fatalf("their tasks = %+v", open)
	}
	if open, _ := f.db.TasksBy(true, "", ram+"/sa", 0); len(open) != 1 {
		t.Fatalf("one session's tasks = %+v", open)
	}
	if !ledger.OwnedBy(ram+"/sa", ram) || ledger.OwnedBy("x"+ram+"/sa", ram) || ledger.OwnedBy(ram, "") {
		t.Fatal("OwnedBy")
	}
}
