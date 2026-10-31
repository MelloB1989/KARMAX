package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karma/ai/jev"
	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/policy"
	"github.com/MelloB1989/karmax/internal/reflex"
	"github.com/MelloB1989/karmax/internal/sources"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// The whole decision core for an account's event, with fakes only at the two
// network edges: TypeSafe (the screener) and the model (the brain). Everything
// between — the router, the turn journal, the mailbox, the screen, filing to
// memory, and the turn's rendering — is the real code.

// recordingBrain is the model: it records what a turn was given.
type recordingBrain struct {
	mu      sync.Mutex
	prompts []string
	ctxs    []string
	pending string
	reply   string
	calls   []karmahelper.ToolCallRecord
	done    chan struct{}
}

func newRecordingBrain() *recordingBrain { return &recordingBrain{done: make(chan struct{}, 8)} }

func (b *recordingBrain) ProcessMessageWithheld(ctx context.Context, msg string, _ []tools.Tool, _ map[string]bool) (string, []karmahelper.ToolCallRecord, error) {
	b.mu.Lock()
	b.prompts = append(b.prompts, msg)
	// The turn context travels with the call; a parked one is the fallback.
	turnCtx := agent.TurnContextFrom(ctx)
	if turnCtx == "" {
		turnCtx = b.pending
	}
	b.ctxs = append(b.ctxs, turnCtx)
	reply, calls := b.reply, b.calls
	b.mu.Unlock()
	b.done <- struct{}{}
	return reply, calls, nil
}
func (b *recordingBrain) SetTurnContext(c string) {
	b.mu.Lock()
	b.pending = c
	b.mu.Unlock()
}
func (b *recordingBrain) NeedsCompaction() bool             { return false }
func (b *recordingBrain) GetHistory() *models.AIChatHistory { return &models.AIChatHistory{} }
func (b *recordingBrain) SetHistory(models.AIChatHistory)   {}
func (b *recordingBrain) GetTotalTokens() int64             { return 0 }
func (b *recordingBrain) GetKeepRecent() int                { return 10 }
func (b *recordingBrain) ResetTokenCount()                  {}

func (b *recordingBrain) turns() (prompts, ctxs []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.prompts...), append([]string(nil), b.ctxs...)
}

// stubScreener answers every screening with one disposition.
func stubScreener(t *testing.T, disposition string, urgency float64) *reflex.Evaluator {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(jev.Result{
			Model: "jev-test",
			Answers: jev.Answers{
				reflex.QDisposition: {Type: jev.TypeChoice, Choice: disposition, Confidence: 0.95,
					Probabilities: map[string]float64{disposition: 0.95}},
				reflex.QUrgency:  {Type: jev.TypeScore, Score: urgency, Confidence: 0.9},
				reflex.QRisk:     {Type: jev.TypeScore, Score: 0, Confidence: 0.9},
				reflex.QRemember: {Type: jev.TypeNoul, Noul: 0.9},
				reflex.QEffort: {Type: jev.TypeChoice, Choice: "normal", Confidence: 0.9,
					Probabilities: map[string]float64{"normal": 0.9}},
			},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	ev, err := reflex.New(reflex.Config{Enabled: true, APIKey: "test-key", BaseURL: srv.URL, Timeout: 2 * time.Second}, zap.NewNop())
	if err != nil || ev == nil {
		t.Fatalf("screener: %v", err)
	}
	return ev
}

type sourceFlow struct {
	rt       *KarmaxRuntime
	a        *agent.Agent
	brain    *recordingBrain
	mem      *memory.Manager
	db       *store.Store
	mu       sync.Mutex
	operator []string // what reached the operator's WhatsApp
}

func (f *sourceFlow) toOperator() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.operator...)
}

func newSourceFlow(t *testing.T, disposition string, urgency float64) *sourceFlow {
	t.Helper()
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "t.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b := bus.NewLog(db, "test", zap.NewNop())
	factory := memory.NewFactory(filepath.Join(dir, "memory"), db, zap.NewNop())
	reg := agent.NewRegistry(b, db, zap.NewNop())
	mem := factory.For("nexus", "")
	a, err := reg.Register(agent.AgentDef{ID: "nexus", Name: "Test agent", Provider: "none", Model: "none"}, mem, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &sourceFlow{a: a, mem: mem, db: db, brain: newRecordingBrain()}
	rt := &KarmaxRuntime{store: db, log: zap.NewNop(), bus: b, agents: reg, memory: factory,
		loopDefaultAgent: "nexus", reflex: stubScreener(t, disposition, urgency), policy: policy.Default(),
		messageOperator: func(text string) error {
			f.mu.Lock()
			f.operator = append(f.operator, text)
			f.mu.Unlock()
			return nil
		}}
	f.rt = rt
	a.SetScreener(rt.screenEvent)
	rt.applyOperatingPolicy(a)
	a.SetHarnessBrain(f.brain)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop() })
	return f
}

// route publishes the event as a source would and hands it to the router, the
// way the bus subscriber does.
func (f *sourceFlow) route(t *testing.T, e sources.Event) bus.Event {
	t.Helper()
	wire, err := json.Marshal(e.BusEvent("nexus"))
	if err != nil {
		t.Fatal(err)
	}
	var evt bus.Event
	if err := json.Unmarshal(wire, &evt); err != nil {
		t.Fatal(err)
	}
	if err := f.rt.routeToAgent(evt); err != nil {
		t.Fatalf("route: %v", err)
	}
	return evt
}

// turnStatus waits for the event's journal row to close.
func (f *sourceFlow) turnStatus(t *testing.T, eventID string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		turns, _ := f.db.RecentTurns("nexus", 20)
		for _, tr := range turns {
			if tr.EventID == eventID && tr.Status != store.TurnRunning {
				return tr.Status
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the turn for %s never finished", eventID)
	return ""
}

func reviewRequest() sources.Event {
	return sources.Event{
		Source: "github", Account: "octo-work", Type: "review_requested", Key: "n-1",
		Title: "Review requested on acme/api#12", Summary: "Dana Reyes wants a review before Friday's release.",
		URL: "https://example.test/acme/api/pull/12", At: time.Now(),
		Data: map[string]any{"repo": "acme/api", "number": 12},
	}
}

// A durable fact is filed to long-term memory under what it is about, and no
// model is woken to do it.
func TestASourceEventScreenedAsRememberIsFiledUnderItsSubject(t *testing.T) {
	f := newSourceFlow(t, "remember", 0)
	evt := f.route(t, reviewRequest())

	if got := f.turnStatus(t, evt.ID); got != store.TurnOK {
		t.Fatalf("turn status %q, want ok", got)
	}
	if prompts, _ := f.brain.turns(); len(prompts) != 0 {
		t.Fatalf("a filed event woke the brain: %v", prompts)
	}
	entries, err := f.mem.Recent(10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("memory holds %d entries (%v), want the one fact", len(entries), err)
	}
	e := entries[0]
	for _, want := range []string{"Review requested on acme/api#12", "Dana Reyes wants a review", "github", "octo-work"} {
		if !strings.Contains(e.Content, want) {
			t.Errorf("the fact omits %q: %q", want, e.Content)
		}
	}
	stored, _ := f.db.SearchMemoryEntries("nexus", "acme/api", 5)
	if len(stored) != 1 || stored[0].Category != "project" {
		t.Fatalf("filed as %+v, want category project", stored)
	}
	if path := memory.PathFor(memory.MemoryEntry{Content: e.Content, Tags: e.Tags, Category: stored[0].Category}, nil); path != "facts/projects/acme-api.md" {
		t.Errorf("filed at %q, want the repository's own file", path)
	}
}

// An email is filed under the person who wrote it, with the people — beside
// everything else known about them — not under the source or the subject
// line. The data is what the Gmail source sets (internal/sources/gmail).
func TestAnEmailScreenedAsRememberIsFiledUnderItsSender(t *testing.T) {
	f := newSourceFlow(t, "remember", 0)
	evt := f.route(t, sources.Event{
		Source: "gmail", Account: "priya@example.com", Type: "message", Key: "18c0a1",
		Title: "Email from Dana Reyes: Lease renewal", Summary: "The renewal starts 1 December; please sign by Friday.",
		URL: "https://mail.google.com/mail/?authuser=priya%40example.com#all/18c0a1", At: time.Now(),
		Data: map[string]any{"person": "Dana Reyes", "category": "people", "from": "Dana Reyes <dana@example.org>",
			"email_subject": "Lease renewal", "thread": "18c0a1"},
	})
	if got := f.turnStatus(t, evt.ID); got != store.TurnOK {
		t.Fatalf("turn status %q, want ok", got)
	}
	entries, err := f.mem.Recent(10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("memory holds %d entries (%v), want the one fact", len(entries), err)
	}
	e := entries[0]
	if !strings.Contains(e.Content, "Dana Reyes") || !strings.Contains(e.Content, "sign by Friday") || !strings.Contains(e.Content, "priya@example.com") {
		t.Errorf("the fact %q", e.Content)
	}
	stored, _ := f.db.SearchMemoryEntries("nexus", "Dana Reyes", 5)
	if len(stored) != 1 || stored[0].Category != "people" {
		t.Fatalf("filed as %+v, want category people", stored)
	}
	if path := memory.PathFor(memory.MemoryEntry{Content: e.Content, Tags: e.Tags, Category: stored[0].Category}, nil); path != "facts/people/dana-reyes.md" {
		t.Errorf("filed at %q, want the sender's own file", path)
	}
}

// A handled event reaches the brain as what happened and on WHICH account, so
// the agent acts with that account's tools — not as a raw JSON dump.
func TestASourceEventScreenedAsHandleReachesTheTurnWithItsAccount(t *testing.T) {
	f := newSourceFlow(t, "handle", 0)
	if err := f.mem.Write(memory.MemoryEntry{Content: "acme/api releases every Friday; Dana Reyes owns the release.",
		Category: "project", Tags: []string{"acme/api"}}); err != nil {
		t.Fatal(err)
	}
	evt := f.route(t, reviewRequest())

	select {
	case <-f.brain.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handled event never reached the brain")
	}
	if got := f.turnStatus(t, evt.ID); got != store.TurnOK {
		t.Fatalf("turn status %q, want ok", got)
	}
	prompts, ctxs := f.brain.turns()
	p := prompts[0]
	for _, want := range []string{"github", "octo-work", "review_requested", "Review requested on acme/api#12",
		"Dana Reyes wants a review", "https://example.test/acme/api/pull/12", "acme/api"} {
		if !strings.Contains(p, want) {
			t.Errorf("the turn omits %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "```json") {
		t.Errorf("the event was rendered as a raw JSON dump:\n%s", p)
	}
	// What memory already holds about the subject comes with the event.
	if !strings.Contains(ctxs[0]+p, "releases every Friday") {
		t.Errorf("related memory did not reach the turn:\nctx:\n%s\nprompt:\n%s", ctxs[0], p)
	}
	// And the turn knows which accounts it acts through, this one included.
	if !strings.Contains(ctxs[0], "Accounts you act through") || !strings.Contains(ctxs[0], "github:octo-work") {
		t.Errorf("the accounts section did not reach the turn:\n%s", ctxs[0])
	}
}

// An urgent event is escalated whatever the screener thought of it, and the
// turn is told so.
func TestAnUrgentSourceEventIsEscalatedToTheTurn(t *testing.T) {
	f := newSourceFlow(t, "drop", 0)
	f.brain.reply = "Paged the on-call and opened an incident."
	e := reviewRequest()
	e.Urgent = true
	e.Title = "CloudWatch alarm api-5xx is in ALARM"
	evt := f.route(t, e)

	select {
	case <-f.brain.done:
	case <-time.After(10 * time.Second):
		t.Fatal("an urgent event was silenced")
	}
	f.turnStatus(t, evt.ID)
	prompts, ctxs := f.brain.turns()
	if !strings.Contains(strings.ToLower(prompts[0]), "urgent") {
		t.Errorf("the turn was not told the event is urgent:\n%s", prompts[0])
	}
	if !strings.Contains(ctxs[0]+prompts[0], "[TRIAGE]") {
		t.Errorf("the escalation did not reach the turn:\n%s", ctxs[0])
	}
	// The operator hears about it once, with what was done.
	got := f.toOperator()
	if len(got) != 1 || !strings.Contains(got[0], "Urgent") || !strings.Contains(got[0], "CloudWatch alarm api-5xx") ||
		!strings.Contains(got[0], "Paged the on-call") {
		t.Errorf("the operator was told %q", got)
	}
}

// A source's first look at an account publishes its backlog — a day of
// GitHub notifications, several marked urgent. At deploy that paged the
// operator once per urgent item. Backlog is handled as context: screened on
// its merits rather than floored to escalate, and never a page.
func TestAFirstSightBacklogDoesNotPageTheOperator(t *testing.T) {
	f := newSourceFlow(t, "handle", 0)
	f.brain.reply = "Already reviewed by Dana; nothing to do."
	var ids []string
	for i, title := range []string{"Review requested on acme/api#12", "Mentioned on acme/web#3", "CI failed on acme/api main"} {
		e := reviewRequest()
		e.Key, e.Title, e.Urgent, e.Backlog = "n-b"+string(rune('0'+i)), title, true, true
		e.At = time.Now().Add(-time.Duration(i+1) * time.Hour)
		ids = append(ids, f.route(t, e).ID)
	}
	for range ids {
		select {
		case <-f.brain.done:
		case <-time.After(10 * time.Second):
			t.Fatal("a backlog item never reached the agent")
		}
	}
	for _, id := range ids {
		f.turnStatus(t, id)
	}
	if got := f.toOperator(); len(got) != 0 {
		t.Errorf("the backlog paged the operator %d times: %q", len(got), got)
	}
	prompts, ctxs := f.brain.turns()
	for i := range prompts {
		if !strings.Contains(prompts[i], "BACKLOG") {
			t.Errorf("turn %d was not told it is backlog:\n%s", i, prompts[i])
		}
		if strings.Contains(ctxs[i], "time-critical") {
			t.Errorf("turn %d was escalated as time-critical:\n%s", i, ctxs[i])
		}
	}
}

// A routine event reaches no one: it is handled, and that is the end of it.
func TestARoutineSourceEventDoesNotReachTheOperator(t *testing.T) {
	f := newSourceFlow(t, "handle", 0)
	evt := f.route(t, reviewRequest())
	<-f.brain.done
	f.turnStatus(t, evt.ID)
	if got := f.toOperator(); len(got) != 0 {
		t.Errorf("a routine event reached the operator's WhatsApp: %q", got)
	}
	// The turn was told the policy it acts under.
	_, ctxs := f.brain.turns()
	if !strings.Contains(ctxs[0], "Operating policy") {
		t.Errorf("the turn was not told the operating policy:\n%s", ctxs[0])
	}
}
