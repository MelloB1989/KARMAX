package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
)

// promptLog is a harness that answers every turn and keeps what it was sent.
type promptLog struct {
	mu   sync.Mutex
	sent []string
}

func (p *promptLog) Send(_ context.Context, _, _, text string) (HarnessTurn, error) {
	p.mu.Lock()
	p.sent = append(p.sent, text)
	p.mu.Unlock()
	return HarnessTurn{Available: true, Text: "ok"}, nil
}

// Conversations run concurrently and share one brain. The turn's context
// used to be parked on the brain (SetTurnContext) and picked up by the next
// call, so two conversations starting together could swap: one chat's turn
// ran with the other's retrieved memory, recent actions and screening. The
// context travels with the call instead.
func TestEachConversationsTurnCarriesItsOwnContext(t *testing.T) {
	send := &promptLog{}
	b := NewHarnessBrain(send, "agent:nexus", "orchestrator", nil)

	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := WithTurnContext(context.Background(), fmt.Sprintf("CONTEXT-OF-%d\n", i))
			_, _, _ = b.ProcessMessageWithheld(ctx, fmt.Sprintf("MESSAGE-OF-%d", i), nil, nil)
		}()
	}
	wg.Wait()

	if len(send.sent) != 40 {
		t.Fatalf("%d turns reached the harness, want 40", len(send.sent))
	}
	for _, p := range send.sent {
		var n int
		if _, err := fmt.Sscanf(p[strings.Index(p, "MESSAGE-OF-"):], "MESSAGE-OF-%d", &n); err != nil {
			t.Fatalf("unreadable prompt %q", p)
		}
		if want := fmt.Sprintf("CONTEXT-OF-%d\n", n); !strings.Contains(p, want) {
			t.Errorf("message %d went out with someone else's context: %q", n, p)
		}
	}
}

// busyBrain is an API session in the middle of another conversation's turn:
// anything that needs its lock waits.
type busyBrain struct{ held chan struct{} }

func (b *busyBrain) SetTurnContext(string) { <-b.held }
func (b *busyBrain) ProcessMessageWithheld(context.Context, string, []tools.Tool, map[string]bool) (string, []karmahelper.ToolCallRecord, error) {
	<-b.held
	return "", nil, nil
}
func (b *busyBrain) NeedsCompaction() bool             { return false }
func (b *busyBrain) GetHistory() *models.AIChatHistory { return &models.AIChatHistory{} }
func (b *busyBrain) SetHistory(models.AIChatHistory)   {}
func (b *busyBrain) GetTotalTokens() int64             { return 0 }
func (b *busyBrain) GetKeepRecent() int                { return 10 }
func (b *busyBrain) ResetTokenCount()                  {}

// Handing the context over used to go through the fallback too, which takes
// the API session's lock — so the orchestrator's turn waited for whatever
// unrelated turn the API session was running, on the turn's own clock.
func TestAHarnessTurnDoesNotWaitOnItsFallbacksLock(t *testing.T) {
	fb := &busyBrain{held: make(chan struct{})}
	defer close(fb.held)
	b := NewHarnessBrain(&promptLog{}, "agent:nexus", "orchestrator", fb)

	done := make(chan struct{})
	go func() {
		ctx := WithTurnContext(context.Background(), "the clock, the profile\n")
		_, _, _ = b.ProcessMessageWithheld(ctx, "hello", nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the harness turn waited on the fallback's lock")
	}
}
