package harness

import (
	"context"
	"os"
	"testing"
	"time"
)

// A kind with no max_turns used to mean a transcript that never ends. The
// health check rides one ("classify"): its "Reply with the single word OK."
// had reached 5,471 turns, and every ping re-read 266k tokens of earlier
// pings — eleven times what the same ping cost the day the session started.
// Every kind is bounded unless the operator says otherwise.
func TestEveryKindHasATurnLimitByDefault(t *testing.T) {
	sup := New(Config{
		Binary: "claude", WorkdirRoot: t.TempDir(), CheapModel: "haiku",
		Policies: map[string]Policy{
			"agent": {Model: "sonnet"},              // configured, max_turns left out
			"chat":  {Model: "sonnet", MaxTurns: 7}, // configured with a limit
			"task":  {Model: "opus", MaxTurns: -1},  // explicitly unbounded
		},
	}, newMemStore(), NewBreaker(0.9, nil), testLog{t}, nil)

	for kind, want := range map[string]int{
		"classify": DefaultMaxTurns, // never configured
		"agent":    DefaultMaxTurns,
		"chat":     7,
		"task":     -1,
	} {
		if got := sup.policy(kind).MaxTurns; got != want {
			t.Errorf("policy(%q).MaxTurns = %d, want %d", kind, got, want)
		}
	}
	if DefaultMaxTurns <= 0 {
		t.Fatalf("DefaultMaxTurns = %d; a default that does not bound anything is the bug", DefaultMaxTurns)
	}
}

// The limit has to reach the resume decision, not just the policy table: a
// session of an unconfigured kind that has done its turns starts over instead
// of reloading the whole transcript again.
func TestAnUnconfiguredKindStartsFreshPastTheDefaultLimit(t *testing.T) {
	st := newMemStore()
	sup := New(Config{
		Binary: writeFakeClaude(t, time.Millisecond), WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		CheapModel: "haiku",
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)

	if _, err := sup.Send(context.Background(), "ping", "classify", "hi"); err != nil {
		t.Fatal(err)
	}
	first, _ := st.GetHarnessSession("ping")
	sup.Close("ping")
	// As if it had been answering a ping every ten minutes for weeks.
	st.mu.Lock()
	st.rows["ping"].Turns = DefaultMaxTurns
	st.mu.Unlock()

	if _, err := sup.Send(context.Background(), "ping", "classify", "hi"); err != nil {
		t.Fatal(err)
	}
	if now, _ := st.GetHarnessSession("ping"); now.HarnessSessionID == first.HarnessSessionID {
		t.Error("resumed the same transcript past the default turn limit; it should start fresh")
	}
}
