package harness

import (
	"context"
	"testing"
	"time"
)

// The decision behind open()'s close-and-respawn, checked without a process:
// a spawn succeeding or failing has nothing to teach it.
func TestNeedsRespawn(t *testing.T) {
	pinned := &Session{Model: "sonnet", Pinned: true, Effort: "high"}
	if needsRespawn(pinned, "sonnet", "high") {
		t.Error("the same model and effort must reuse the session")
	}
	if !needsRespawn(pinned, "opus", "high") {
		t.Error("a different model must respawn")
	}
	if !needsRespawn(pinned, "sonnet", "low") {
		t.Error("a different effort must respawn")
	}
	if !needsRespawn(pinned, "sonnet", "") {
		t.Error("dropping the effort must respawn a session spawned with one")
	}
	if !needsRespawn(pinned, "", "high") {
		t.Error("asking for no model must undo an earlier turn's pin")
	}

	// The breaker moves the policy's model between turns; a session it chose
	// is kept warm through that, which is the whole point of degrading.
	chosen := &Session{Model: "haiku"}
	if needsRespawn(chosen, "", "") {
		t.Error("a turn naming no model must not restart a session the policy chose")
	}
}

// A policy that has moved to another model since the session opened leaves
// the warm process in place.
func TestOpenKeepsAWarmSessionWhenOnlyThePolicyMoves(t *testing.T) {
	sup, _ := newRaceSupervisor(t, 5*time.Millisecond)
	ctx := context.Background()

	first, err := sup.open(ctx, "k", "chat", sup.policy("chat"), Options{})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := first.Send(ctx, "warm up", 5*time.Second, nil); err != nil {
		t.Fatalf("warm-up turn: %v", err)
	}

	degraded := sup.policy("chat")
	degraded.Model = first.Model + "-degraded"
	second, err := sup.open(ctx, "k", "chat", degraded, Options{})
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if second != first {
		t.Error("a degraded policy should have kept the warm session, not respawned it")
	}
}

// A turn asking for the same model and effort the live process already runs
// on gets that exact process back: no restart, no cold start paid for
// nothing.
func TestOpenReusesALiveSessionWithMatchingFlags(t *testing.T) {
	sup, _ := newRaceSupervisor(t, 5*time.Millisecond)
	ctx := context.Background()

	first, err := sup.open(ctx, "k", "chat", sup.policy("chat"), Options{Model: "sonnet", Effort: "high"})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := first.Send(ctx, "warm up", 5*time.Second, nil); err != nil {
		t.Fatalf("warm-up turn: %v", err)
	}

	second, err := sup.open(ctx, "k", "chat", sup.policy("chat"), Options{Model: "sonnet", Effort: "high"})
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if second != first {
		t.Error("matching model and effort should have reused the live session, not respawned")
	}
}

// A turn asking for a different model or effort than the live process was
// spawned with gets a NEW process, on the SAME CLI session id — the
// conversation continues (--resume), only the flags change.
func TestOpenRespawnsWhenFlagsChange(t *testing.T) {
	sup, _ := newRaceSupervisor(t, 5*time.Millisecond)
	ctx := context.Background()

	first, err := sup.open(ctx, "k", "chat", sup.policy("chat"), Options{Model: "sonnet"})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := first.Send(ctx, "warm up", 5*time.Second, nil); err != nil {
		t.Fatalf("warm-up turn: %v", err)
	}
	firstID := first.ID

	second, err := sup.open(ctx, "k", "chat", sup.policy("chat"), Options{Model: "sonnet", Effort: "high"})
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if second == first {
		t.Fatal("a changed effort should have respawned, not returned the same session")
	}
	if second.ID != firstID {
		t.Errorf("respawn lost the conversation: session id = %q, want %q", second.ID, firstID)
	}
	if second.Effort != "high" {
		t.Errorf("the respawned session did not get the new effort: %q", second.Effort)
	}
	if first.Alive() {
		t.Error("the old process should have been closed, not left running alongside the new one")
	}
}
