package harness

import (
	"context"
	"slices"
	"testing"
)

// Step 1 evidence: a kind nobody configured resolves to an empty Policy.Model,
// and spawnArgs only appends --model when one is set (session.go) -- so today
// it spawns a bare `claude` and rides whatever that build defaults to. This is
// the live profile's own shape: it configures chat/agent/task and nothing
// else, while loophost.go and harnesshost.go ask for "gateway" and "summary".
//
// Asserted at two levels on purpose: policy() for the resolved value, and
// spawnArgs() for the real argument list it produces -- the thing that
// actually reaches the process.
func TestAnUnconfiguredKindGetsTheCheapModel(t *testing.T) {
	sup := New(Config{
		Binary: "claude", WorkdirRoot: t.TempDir(),
		CheapModel: "haiku",
		Policies:   map[string]Policy{"chat": {Model: "sonnet"}},
	}, newMemStore(), NewBreaker(0.9, nil), testLog{t}, nil)

	pol := sup.policy("gateway") // never in Policies, same as the live profile
	if pol.Model != "haiku" {
		t.Fatalf("policy(\"gateway\").Model = %q, want the cheap tier", pol.Model)
	}

	args := spawnArgs(&Session{ID: "s", Model: pol.Model}, false, "")
	i := slices.Index(args, "--model")
	if i < 0 || args[i+1] != "haiku" {
		t.Errorf("spawn args = %v, want --model haiku", args)
	}
}

// Regression guard: an operator who explicitly pinned a kind must keep
// exactly what they asked for -- the cheap default only fills a gap, it never
// overrides a configured value.
func TestAConfiguredKindKeepsItsOwnModel(t *testing.T) {
	sup := New(Config{
		Binary: "claude", WorkdirRoot: t.TempDir(),
		CheapModel: "haiku",
		Policies:   map[string]Policy{"agent": {Model: "sonnet"}},
	}, newMemStore(), NewBreaker(0.9, nil), testLog{t}, nil)

	pol := sup.policy("agent")
	if pol.Model != "sonnet" {
		t.Errorf("policy(\"agent\").Model = %q, want the operator's own sonnet, not the cheap fallback", pol.Model)
	}
}

// Regression guard: a caller that names a model for this one turn must still
// get it, even on a kind that would otherwise fall to the cheap default. The
// breaker's degrade path (supervisor.go) and the --model flag (session.go)
// both depend on Options.Model being the final word.
func TestAnExplicitPerTurnModelStillWins(t *testing.T) {
	st := newMemStore()
	sup := New(Config{
		Binary: "definitely-not-a-real-binary-xyz", WorkdirRoot: t.TempDir(),
		CheapModel: "haiku",
	}, st, NewBreaker(0.9, nil), testLog{t}, nil)

	// "classify" is unconfigured, so its default is the cheap tier -- but this
	// call asks for something specific, and that must win.
	_, _ = sup.SendWith(context.Background(), "c", "classify", "hello", Options{Model: "opus"})
	rec, _ := st.GetHarnessSession("c")
	if rec == nil {
		t.Fatal("no session was opened at all")
	}
	if rec.Model != "opus" {
		t.Errorf("explicit per-turn model = %q, want opus to win over the unconfigured-kind default", rec.Model)
	}
}
