package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// logEntries is what the loops have filed in their logs, oldest first.
func logEntries(t *testing.T, rt *KarmaxRuntime) []store.StoredMemoryEntry {
	t.Helper()
	all, err := rt.store.ListMemoryEntries(rt.loopDefaultAgent, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.StoredMemoryEntry
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Category == memory.CategoryLog {
			out = append(out, all[i])
		}
	}
	return out
}

// runOnce runs a loop to completion.
func runOnce(rt *KarmaxRuntime, l loopkit.Loop) {
	rt.runLoopDurable(context.Background(), l, loopkit.Trigger{Kind: loopkit.TriggerSchedule}, 1)
}

// stamped is a line as the loops write them, stamped with when it was said.
func stamped(loop, line string) string {
	return "(" + loop + ", " + time.Now().Format("2 Jan 15:04") + ") " + line
}

// A loop's run is one write to its log, however many lines it remembers — and
// the lines are filed even when the run fails after saying them. One write per
// line was 343 of a night's 550 writes, and every write makes the store
// re-upload the whole repository.
func TestARunsLogIsOneWriteEvenWhenTheRunFails(t *testing.T) {
	rt := loopRuntime(t)
	l := loopkit.Loop{Name: "night-watch", Run: func(ctx context.Context, k loopkit.Kit) error {
		for _, line := range []string{"group-a [waiting]: Omar owes the deck", "group-b [flagged]: Priya asked for the invoice", "group-c [waiting]: the venue has not replied"} {
			if err := k.Remember(stamped("night-watch", line)); err != nil {
				return err
			}
		}
		return errors.New("the chat API went away")
	}}
	runOnce(rt, l)

	got := logEntries(t, rt)
	if len(got) != 1 {
		t.Fatalf("the run made %d writes to its log, want 1", len(got))
	}
	for _, want := range []string{"Omar owes the deck", "Priya asked for the invoice", "the venue has not replied"} {
		if !strings.Contains(got[0].Content, want) {
			t.Errorf("%q is not in the run's log entry:\n%s", want, got[0].Content)
		}
	}
}

// A signed (WASM) loop's remember host call is batched the same way.
func TestASignedLoopsRememberIsBatchedToo(t *testing.T) {
	rt := loopRuntime(t)
	w := &wasmKit{rt: rt, loop: "chat-review"}
	l := loopkit.Loop{Name: "chat-review", Run: func(ctx context.Context, k loopkit.Kit) error {
		_ = w.Remember(stamped("chat-review", "group-a [waiting]: Omar owes the deck"))
		_ = w.Remember(stamped("chat-review", "group-b [flagged]: Priya asked for the invoice"))
		return nil
	}}
	runOnce(rt, l)
	if got := logEntries(t, rt); len(got) != 1 {
		t.Fatalf("the run made %d writes to its log, want 1", len(got))
	}

	// Outside a run — a tool the loop lends the agent — a line is written as said.
	if err := w.Remember("(chat-review) the agent asked for a summary"); err != nil {
		t.Fatal(err)
	}
	if got := logEntries(t, rt); len(got) != 2 {
		t.Errorf("a line remembered outside a run was not written (%d entries)", len(got))
	}
}

// The daemon dies mid-run (the machine lost power twice in one night). What
// the run had said is not lost with it: the loop's next run files it first,
// as the interrupted run's.
func TestTheLogOfARunTheDaemonDiedInIsFiledByTheNextRun(t *testing.T) {
	rt := loopRuntime(t)
	cut := rt.beginRunLog("night-watch")
	if held, err := rt.logLine("night-watch", stamped("night-watch", "group-a [flagged]: Omar cancelled the call")); !held || err != nil {
		t.Fatalf("logLine = %v, %v", held, err)
	}
	_ = cut // the process ends here; endRunLog never runs

	after := &KarmaxRuntime{store: rt.store, log: rt.log, cfg: rt.cfg, memory: rt.memory,
		pending: newPendingTriggers(), loopDefaultAgent: rt.loopDefaultAgent}
	runOnce(after, loopkit.Loop{Name: "night-watch", Run: func(ctx context.Context, k loopkit.Kit) error {
		return k.Remember(stamped("night-watch", "group-b [waiting]: the venue has not replied"))
	}})

	got := logEntries(t, after)
	if len(got) != 2 {
		t.Fatalf("%d log entries, want the interrupted run's and this run's", len(got))
	}
	all := got[0].Content + "\n" + got[1].Content
	for _, want := range []string{"Omar cancelled the call", "the venue has not replied"} {
		if !strings.Contains(all, want) {
			t.Errorf("%q was lost", want)
		}
	}
}

// A line that says what the loop's previous run said is not news, and a run
// with nothing new to say writes nothing.
func TestARunThatSaysNothingNewWritesNothing(t *testing.T) {
	rt := loopRuntime(t)
	lines := []string{"group-a [waiting]: Omar owes the deck", "group-b [waiting]: the venue has not replied"}
	l := loopkit.Loop{Name: "night-watch", Run: func(ctx context.Context, k loopkit.Kit) error {
		for _, line := range lines {
			_ = k.Remember(stamped("night-watch", line))
		}
		return nil
	}}
	runOnce(rt, l)
	runOnce(rt, l)
	if got := logEntries(t, rt); len(got) != 1 {
		t.Fatalf("%d log entries after a run that said nothing new, want 1", len(got))
	}

	lines[1] = "group-b [flagged]: the venue cancelled"
	runOnce(rt, l)
	got := logEntries(t, rt)
	if len(got) != 2 {
		t.Fatalf("%d log entries, want a second for the run that said something new", len(got))
	}
	// Written within a second of each other, so found by what they say.
	found := false
	for _, e := range got {
		if !strings.Contains(e.Content, "the venue cancelled") {
			continue
		}
		found = true
		if strings.Contains(e.Content, "Omar owes the deck") {
			t.Errorf("the new entry should hold only what changed:\n%s", e.Content)
		}
	}
	if !found {
		t.Error("what changed was not filed")
	}
}
