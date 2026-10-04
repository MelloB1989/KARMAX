package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/voice"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "t.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type fakeSession struct {
	reply string
	err   error
	asked []string
	hist  models.AIChatHistory
}

func (f *fakeSession) Chat(_ context.Context, m string) (string, []karmahelper.ToolCallRecord, karmahelper.TokenInfo, error) {
	f.asked = append(f.asked, m)
	return f.reply, nil, karmahelper.TokenInfo{}, f.err
}
func (f *fakeSession) SetHistory(h models.AIChatHistory)            { f.hist = h }
func (f *fakeSession) GetHistory() models.AIChatHistory             { return f.hist }
func (f *fakeSession) SetContext(string)                            {}
func (f *fakeSession) PrimeTurn(string) func(context.Context) error { return nil }

func newTestBrain(sess voiceSession, l *voiceLedger) *voiceBrain {
	return &voiceBrain{session: sess, ledger: l, notices: make(chan voice.Reply, 4),
		done: make(chan struct{}), log: zap.NewNop()}
}

func TestLedgerAccountsAndKeysByFingerprint(t *testing.T) {
	s := testStore(t)
	l := newVoiceLedger(s, "secret-key", 20, []string{"qwen.qwen3-next-80b-a3b"}, nil, zap.NewNop())
	usd := l.record(karmahelper.Usage{Provider: "bedrock", Model: "qwen.qwen3-next-80b-a3b", InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if usd < 1.34 || usd > 1.36 {
		t.Fatalf("cost = %v, want 1.35", usd)
	}
	if l.record(karmahelper.Usage{Provider: "claude-code", Model: "haiku", InputTokens: 9e6}) != 0 {
		t.Fatal("non-bedrock spend counted against the key")
	}
	got, _ := s.VoiceSpent(keyFingerprint("secret-key"))
	if got != usd {
		t.Fatalf("ledger holds %v, want %v", got, usd)
	}
	if fp := keyFingerprint("secret-key"); strings.Contains(fp, "secret") || len(fp) != 16 {
		t.Fatalf("bad fingerprint %q", fp)
	}
}

func TestOverCapTurnHangsUpAndAlertsOnce(t *testing.T) {
	s := testStore(t)
	var alerts []string
	var mu sync.Mutex
	l := newVoiceLedger(s, "k", 1, []string{"qwen.qwen3-next-80b-a3b"}, func(m string) error {
		mu.Lock()
		alerts = append(alerts, m)
		mu.Unlock()
		return nil
	}, zap.NewNop())
	sess := &fakeSession{reply: "hi"}
	b := newTestBrain(sess, l)

	r, err := b.Answer(context.Background(), voice.Utterance{Text: "hello"})
	if err != nil || r.Hangup || len(sess.asked) != 1 {
		t.Fatalf("under cap should answer: %+v %v", r, err)
	}
	l.record(karmahelper.Usage{Provider: "bedrock", Model: "qwen.qwen3-next-80b-a3b", InputTokens: 10_000_000})
	for i := 0; i < 3; i++ {
		r, _ = b.Answer(context.Background(), voice.Utterance{Text: "again"})
		if !r.Hangup || r.Text != voiceBudgetLine {
			t.Fatalf("over cap must speak the budget line and hang up: %+v", r)
		}
	}
	if len(sess.asked) != 1 {
		t.Fatal("the model ran on an over-cap turn")
	}
	if len(alerts) != 1 {
		t.Fatalf("want exactly one alert per crossing, got %d", len(alerts))
	}
	if g := newTestBrain(sess, l); g.Greeting(context.Background(), "p") != "" {
		t.Fatal("greeting should be withheld over the cap")
	}
}

func TestGreetingOverCapQueuesHangup(t *testing.T) {
	s := testStore(t)
	l := newVoiceLedger(s, "k", 0.0001, []string{"qwen.qwen3-next-80b-a3b"}, nil, zap.NewNop())
	b := newTestBrain(&fakeSession{}, l)
	if b.Greeting(context.Background(), "p") != "" {
		t.Fatal("spoke a greeting over the cap")
	}
	if n := <-b.notices; !n.Hangup || n.Text != voiceBudgetLine {
		t.Fatalf("notice = %+v", n)
	}
}

type namedTool struct{ name string }

func (n namedTool) Manifest() tools.ToolManifest { return tools.ToolManifest{Name: n.name} }
func (n namedTool) Execute(context.Context, map[string]any) (tools.ToolResult, error) {
	return tools.SuccessResult(nil), nil
}

func TestOperatorOnlyToolGating(t *testing.T) {
	common := []tools.Tool{namedTool{"call.hangup"}}
	priv := []tools.Tool{namedTool{"task.create"}, namedTool{"task.list"}, namedTool{"memory.ingest"}, namedTool{"orchestrator.send"}}
	names := func(ts []tools.Tool) string {
		var n []string
		for _, t := range ts {
			n = append(n, t.Manifest().Name)
		}
		return strings.Join(n, ",")
	}
	if got := names(voiceToolSet(false, common, priv)); got != "call.hangup" {
		t.Fatalf("non-operator got %s", got)
	}
	if got := names(voiceToolSet(true, common, priv)); !strings.Contains(got, "task.create") || !strings.Contains(got, "memory.ingest") {
		t.Fatalf("operator got %s", got)
	}
	isOp := func(p string) bool { return p == "op" }
	if voiceCallerIsOperator("", func(string) bool { return true }) {
		t.Fatal("empty peer must never be the operator")
	}
	if !voiceCallerIsOperator("op", isOp) || voiceCallerIsOperator("stranger", isOp) {
		t.Fatal("gate misjudged the peer")
	}
}

func TestVoiceTaskCreateForcesOperatorChannel(t *testing.T) {
	s := testStore(t)
	rt := &KarmaxRuntime{store: s, log: zap.NewNop()}
	var notices []string
	tool := &voiceTaskCreateTool{
		inner:     &taskCreateTool{ref: &harnessRef{rt: rt}, agentID: "a"},
		channelID: "wa-1", target: "op-dm",
		notify: func(m string) error { notices = append(notices, m); return nil },
	}
	if strings.Contains(string(tool.Manifest().Parameters), "channel_id") {
		t.Fatal("the model can still choose where a task reports")
	}
	res, err := tool.Execute(context.Background(), map[string]any{"goal": "do x", "title": "X", "channel_id": "evil", "target": "evil"})
	if err != nil || res.IsError {
		t.Fatalf("%+v %v", res, err)
	}
	rows, _ := s.ListTasks("", 5)
	if len(rows) != 1 || rows[0].ChannelID != "wa-1" || rows[0].Target != "op-dm" {
		t.Fatalf("task = %+v", rows)
	}
	if len(notices) != 1 || notices[0] != "Task started: X" {
		t.Fatalf("notices = %v", notices)
	}
}

func TestBriefDrivesFirstTurn(t *testing.T) {
	sess := &fakeSession{reply: "Hi, quick check on the deploy - can it go out today?"}
	b := newTestBrain(sess, nil)
	b.callBrief = "ask whether the deploy can go out"
	got := b.Greeting(context.Background(), "p")
	if got != "Hi, quick check on the deploy - can it go out today?" {
		t.Fatalf("greeting = %q", got)
	}
	if len(sess.asked) != 1 || !strings.Contains(sess.asked[0], "ask whether the deploy can go out") {
		t.Fatalf("model not given the brief: %v", sess.asked)
	}
	// Failure falls back to the plain greeting.
	b2 := newTestBrain(&fakeSession{err: errors.New("down")}, nil)
	b2.callBrief = "x"
	if b2.Greeting(context.Background(), "p") == "" {
		t.Fatal("no greeting after a failed brief turn")
	}
	// No brief: the model is not consulted.
	s3 := &fakeSession{}
	if newTestBrain(s3, nil).Greeting(context.Background(), "p") == "" || len(s3.asked) != 0 {
		t.Fatal("plain greeting should not call the model")
	}
}

func TestReportTaskJevGate(t *testing.T) {
	task := store.Task{ID: "t1", Title: "T", Goal: "g", ChannelID: "c", Target: "d"}
	run := func(dec func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error)) []string {
		var sent []string
		rt := &KarmaxRuntime{log: zap.NewNop()}
		rt.taskHooks.decide = dec
		rt.taskHooks.send = func(_, _, text string) error { sent = append(sent, text); return nil }
		rt.reportTask(task, store.TaskWorking, "halfway there")
		return sent
	}
	answer := func(p float64) func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error) {
		return func(_ context.Context, state any, _ loopkit.Questions) (*loopkit.Decision, error) {
			m := state.(map[string]any)
			if m["goal"] != "g" || m["status"] != store.TaskWorking || m["latest_update"] != "halfway there" {
				t.Errorf("state = %v", m)
			}
			if _, ok := m["minutes_since_last_report"]; !ok {
				t.Error("no minutes_since_last_report")
			}
			return &loopkit.Decision{Answers: map[string]loopkit.Answer{"send_update": {Noul: p}}}, nil
		}
	}
	if got := run(answer(0.9)); len(got) != 1 {
		t.Fatalf("yes verdict: sent %v", got)
	}
	if got := run(answer(0.1)); len(got) != 0 {
		t.Fatalf("no verdict: sent %v", got)
	}
	unavailable := func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error) {
		return nil, errors.New("jev down")
	}
	if got := run(unavailable); len(got) != 1 {
		t.Fatalf("jev unavailable must fail open: sent %v", got)
	}
}
