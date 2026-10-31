package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
	"github.com/MelloB1989/karmax/internal/store"
)

// A message on a Chat account: a conversation a memory loop reads.
func chatMessage() sources.Event {
	return sources.Event{
		Source: "gchat", Account: "rowan@example.test", Type: "message",
		Key:     "gchat|rowan@example.test|spaces/AAAA/messages/1",
		Title:   "Launch crew — Mateo Ruiz: the launch moved to 2026-11-12",
		Summary: "From: Mateo Ruiz\nWhere: Launch crew (space)\nMessage:\nthe launch moved to 2026-11-12",
		At:      time.Now(), Data: map[string]any{"space": "spaces/AAAA", "sender_name": "Mateo Ruiz"},
	}
}

// Memory sees a conversation once. A message from a source whose
// conversations a memory loop reads is filed by that loop; the screen's
// "remember" files nothing of its own, and no model is woken to do it.
// A source no loop reads is filed as before.
func TestAMessageAMemoryLoopReadsIsNotFiledAgainByTheScreen(t *testing.T) {
	f := newSourceFlow(t, "remember", 0)
	f.rt.memorySynced = &sources.MemorySync{}
	f.rt.memorySynced.Cover("gchat")

	evt := f.route(t, chatMessage())
	if got := f.turnStatus(t, evt.ID); got != store.TurnOK {
		t.Fatalf("turn status %q, want ok", got)
	}
	if prompts, _ := f.brain.turns(); len(prompts) != 0 {
		t.Fatalf("a remembered event woke the brain: %v", prompts)
	}
	if entries, _ := f.mem.Recent(10); len(entries) != 0 {
		t.Fatalf("memory holds %d entries; the memory loop files this conversation, not the screen", len(entries))
	}

	other := f.route(t, reviewRequest())
	f.turnStatus(t, other.ID)
	if entries, _ := f.mem.Recent(10); len(entries) != 1 {
		t.Errorf("memory holds %d entries; a source no loop reads is still filed", len(entries))
	}
}

// The same message, handled, reaches the turn to be acted on, and the turn is
// told not to file what it says.
func TestAMessageAMemoryLoopReadsReachesTheTurnWithoutFiling(t *testing.T) {
	f := newSourceFlow(t, "handle", 0)
	f.rt.memorySynced = &sources.MemorySync{}
	f.rt.memorySynced.Cover("gchat")

	evt := f.route(t, chatMessage())
	select {
	case <-f.brain.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handled event never reached the brain")
	}
	f.turnStatus(t, evt.ID)
	prompts, _ := f.brain.turns()
	p := prompts[0]
	if !strings.Contains(p, "Do not file what this message says") || strings.Contains(p, "File what is durable") {
		t.Errorf("the turn was not told its conversation is already read into memory:\n%s", p)
	}
	if !strings.Contains(p, "the launch moved to 2026-11-12") {
		t.Errorf("the message did not reach the turn:\n%s", p)
	}
}
