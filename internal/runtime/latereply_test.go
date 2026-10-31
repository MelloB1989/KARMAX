package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// gaveUpBrain is a harness turn that runs past its deadline: the call fails,
// and it keeps the handler for the rest of the turn its call carried.
type gaveUpBrain struct {
	mu   sync.Mutex
	late func(agent.HarnessTurn)
	done chan struct{}
}

func (b *gaveUpBrain) ProcessMessageWithheld(ctx context.Context, _ string, _ []tools.Tool, _ map[string]bool) (string, []karmahelper.ToolCallRecord, error) {
	b.mu.Lock()
	b.late = agent.LateReplyFrom(ctx)
	b.mu.Unlock()
	b.done <- struct{}{}
	return "", nil, errors.New("harness turn timed out after 3m0s")
}
func (b *gaveUpBrain) SetTurnContext(string)             {}
func (b *gaveUpBrain) NeedsCompaction() bool             { return false }
func (b *gaveUpBrain) GetHistory() *models.AIChatHistory { return &models.AIChatHistory{} }
func (b *gaveUpBrain) SetHistory(models.AIChatHistory)   {}
func (b *gaveUpBrain) GetTotalTokens() int64             { return 0 }
func (b *gaveUpBrain) GetKeepRecent() int                { return 10 }
func (b *gaveUpBrain) ResetTokenCount()                  {}

// An urgent event's turn timed out; the session finished it anyway. The
// operator was paged when the turn failed, and its result follows the page
// instead of vanishing on the bus.
func TestTheRestOfAnUrgentEventsTurnReachesTheOperator(t *testing.T) {
	f := newSourceFlow(t, "handle", 0)
	b := &gaveUpBrain{done: make(chan struct{}, 2)}
	f.a.SetHarnessBrain(b)
	e := reviewRequest()
	e.Urgent = true
	e.Title = "CloudWatch alarm api-5xx is in ALARM"
	evt := f.route(t, e)

	select {
	case <-b.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never ran")
	}
	if got := f.turnStatus(t, evt.ID); got != store.TurnFailed {
		t.Fatalf("turn status %q, want failed", got)
	}
	b.mu.Lock()
	late := b.late
	b.mu.Unlock()
	if late == nil {
		t.Fatal("the turn's harness call carried no handler for its late rest")
	}
	// The failed turn's report is sent just after its journal row closes.
	if got := waitForOperator(f, 1); len(got) != 1 || !strings.Contains(got[0], "Urgent") {
		t.Fatalf("when the turn failed the operator was told %q, want the urgent page", got)
	}

	late(agent.HarnessTurn{Available: true, Text: "Paged the on-call and opened an incident."})

	got := f.toOperator()
	if len(got) != 2 || !strings.Contains(got[1], "CloudWatch alarm api-5xx") || !strings.Contains(got[1], "Paged the on-call") {
		t.Fatalf("the late result did not reach the operator: %q", got)
	}
}

// waitForOperator waits until n messages have reached the operator, or a
// few seconds pass, and returns what did.
func waitForOperator(f *sourceFlow, n int) []string {
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := f.toOperator()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lateClaude answers "slow" from a subshell that outlives stdin closing — a
// launched (docker exec) session finishing its turn after the caller gave up
// and the session was asked to close.
func lateClaude(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "late.sh")
	body := "#!/bin/bash\n" +
		"while IFS= read -r line; do\n" +
		"  ( sleep 0.4\n" +
		"    echo '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"Shipped the export page.\"}]}}'\n" +
		"    echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"Shipped the export page.\",\"usage\":{}}' ) &\n" +
		"done\nwait\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The whole way: an agent's call carries its late-reply handler, the sender
// hands it to the session, the call gives up, and the rest of the turn comes
// back to that handler rather than to nobody.
func TestTheRestOfATurnComesBackThroughTheSender(t *testing.T) {
	db, err := store.New(filepath.Join(t.TempDir(), "t.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var mu sync.Mutex
	var bg []harness.Turn
	sup := harness.New(harness.Config{
		Binary: lateClaude(t), WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		Policies: map[string]harness.Policy{"orchestrator": {TurnTimeout: 5 * time.Second, Launch: []string{"env"}}},
		OnBackgroundTurn: func(_, _ string, t harness.Turn) {
			mu.Lock()
			bg = append(bg, t)
			mu.Unlock()
		},
	}, harnessStore{db}, harness.NewBreaker(0.95, nil), harnessLog{zap.NewNop()}, nil)
	t.Cleanup(sup.Shutdown)

	got := make(chan agent.HarnessTurn, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	ctx = agent.WithLateReply(ctx, func(t agent.HarnessTurn) { got <- t })
	send := harnessSenderFor{sup: sup, prompts: map[string]string{}}
	if _, err := send.Send(ctx, "agent:nexus", "orchestrator", "build the export page"); err == nil {
		t.Fatal("the call outlived its deadline")
	}
	select {
	case turn := <-got:
		if turn.Text != "Shipped the export page." {
			t.Errorf("late turn = %+v", turn)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the rest of the turn never came back to the agent's handler")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bg) != 0 {
		t.Errorf("a claimed late turn also went to the background handler: %+v", bg)
	}
}

// The adapter hands the agent's late-reply handler to the harness, and the
// harness's turn back to it in the agent's terms.
func TestTheLateReplyHandlerCrossesToTheHarness(t *testing.T) {
	if lateReplyTo(nil) != nil {
		t.Fatal("no handler became a handler")
	}
	var got []agent.HarnessTurn
	fn := lateReplyTo(func(t agent.HarnessTurn) { got = append(got, t) })
	fn(harness.Turn{Text: "done", Late: true, ToolCalls: []harness.ToolCall{{Name: "Bash"}}})
	if len(got) != 1 || got[0].Text != "done" || !got[0].Available || len(got[0].ToolCalls) != 1 {
		t.Errorf("the agent got %+v", got)
	}
}

// A turn an agent's session ran that no conversation claimed — a peer's
// message it answered — reaches the operator per policy through that agent,
// instead of only the bus. A session that is not an agent's keeps its turn
// with its workflow.
func TestAnUnclaimedTurnOfAnAgentsSessionIsReported(t *testing.T) {
	f := newSourceFlow(t, "handle", 0)

	f.rt.reportBackgroundTurn("agent:nexus", harness.Turn{Text: "agent-03 finished the export fix.", Duration: time.Minute})
	f.rt.reportBackgroundTurn("extract:wa-memory", harness.Turn{Text: `{"facts": []}`})

	notes, _ := f.db.ListNotifications(10)
	var bodies []string
	for _, n := range notes {
		bodies = append(bodies, n.Body)
	}
	all := strings.Join(bodies, "\n")
	if !strings.Contains(all, "agent-03 finished the export fix.") {
		t.Errorf("the agent's background turn is not in the app: %q", bodies)
	}
	if strings.Contains(all, "facts") {
		t.Errorf("a workflow session's turn was reported to the operator: %q", bodies)
	}
}

// What anything watching the bus sees of a background turn says whether it
// is the rest of a turn its caller gave up on.
func TestABackgroundTurnEventSaysWhetherItIsLate(t *testing.T) {
	if ev := backgroundTurnEvent("agent:nexus", "agent", harness.Turn{Text: "x", Late: true}); ev.Payload["late"] != true {
		t.Errorf("payload = %v, want late", ev.Payload)
	}
	if ev := backgroundTurnEvent("agent:nexus", "agent", harness.Turn{Text: "x"}); ev.Payload["late"] != false {
		t.Errorf("payload = %v, want not late", ev.Payload)
	}
}
