package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

func TestFollowupRelaysToTaskAgent(t *testing.T) {
	f, boxes, _ := newFleet(t)
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	if err := f.Followup(context.Background(), "T1", "karmax", "also cover the empty case", t0); err != nil {
		t.Fatal(err)
	}
	want := "relay karmax→agent-03: Task T1 follow-up from karmax. also cover the empty case"
	if !slices.Contains(boxes["agent-03"].calls, want) {
		t.Fatalf("calls = %q, want %q", boxes["agent-03"].calls, want)
	}
}

func TestFollowupClosedTask409(t *testing.T) {
	f, boxes, _ := newFleet(t)
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	_ = f.Close("agent-03", "T1", "done", "", "", t0)
	err := f.Followup(context.Background(), "T1", "karmax", "more", t0)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "task T1 is closed") {
		t.Fatalf("err = %v", err)
	}
	if len(boxes["agent-03"].calls) != 0 {
		t.Fatalf("typed into the agent: %q", boxes["agent-03"].calls)
	}
}

func TestFollowupUnknownTask404(t *testing.T) {
	f, _, _ := newFleet(t)
	if err := f.Followup(context.Background(), "nope", "karmax", "x", t0); !errors.Is(err, ErrNoTask) {
		t.Fatalf("err = %v", err)
	}
	if err := f.Cancel(context.Background(), "nope", "karmax", "", t0); !errors.Is(err, ErrNoTask) {
		t.Fatalf("cancel err = %v", err)
	}
}

func TestCancelInterruptsTellsCloses(t *testing.T) {
	f, boxes, _ := newFleet(t)
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	boxes["agent-03"].calls = nil
	if err := f.Cancel(context.Background(), "T1", "karmax", "wrong repo", t0); err != nil {
		t.Fatal(err)
	}
	want := []string{"interrupt",
		"relay karmax→agent-03: Task T1 was cancelled by karmax: stop work on it now, leave the branch as it is, and report nothing further. wrong repo"}
	if !slices.Equal(boxes["agent-03"].calls, want) {
		t.Fatalf("calls = %q", boxes["agent-03"].calls)
	}
	tk, _ := f.db.Task("T1")
	if tk.Closed.IsZero() || tk.Outcome != "cancelled" {
		t.Fatalf("task = %+v", tk)
	}
	for _, c := range boxes["agent-03"].calls {
		if c == "fresh" {
			t.Fatal("cancel must keep the session")
		}
	}
}

func TestCancelInterruptFailureStillClosesAndReports(t *testing.T) {
	f, boxes, _ := newFleet(t)
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	boxes["agent-03"].interruptErr = errors.New("no pane")
	err := f.Cancel(context.Background(), "T1", "karmax", "", t0)
	if err == nil || !strings.Contains(err.Error(), "no pane") {
		t.Fatalf("err = %v", err)
	}
	if tk, _ := f.db.Task("T1"); tk.Closed.IsZero() {
		t.Fatal("task not closed after an interrupt failure")
	}
}

func TestCancelClosedTaskDoesNothing(t *testing.T) {
	f, boxes, _ := newFleet(t)
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	_ = f.Close("agent-03", "T1", "done", "", "", t0)
	boxes["agent-03"].calls = nil
	err := f.Cancel(context.Background(), "T1", "karmax", "", t0)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	if len(boxes["agent-03"].calls) != 0 {
		t.Fatalf("typed into the agent: %q", boxes["agent-03"].calls)
	}
	if tk, _ := f.db.Task("T1"); tk.Outcome != "done" {
		t.Fatalf("outcome changed: %+v", tk)
	}
}

func TestDoneByTaskResolvesAgent(t *testing.T) {
	f, _, _ := newFleet(t)
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	if err := f.Done("", "T1", "", "", "", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if tk, _ := f.db.Task("T1"); tk.Closed.IsZero() || tk.Outcome != "done" {
		t.Fatalf("task = %+v", tk)
	}
	if !f.State("agent-03").Done {
		t.Fatal("agent not marked done")
	}
	if err := f.Done("", "nope", "", "", "", t0); !errors.Is(err, ErrNoTask) {
		t.Fatalf("unknown task err = %v", err)
	}
	if err := f.Close("", "nope", "", "", "", t0); !errors.Is(err, ErrNoTask) {
		t.Fatalf("close unknown task err = %v", err)
	}
	if err := f.Done("", "", "", "", "", t0); err == nil {
		t.Fatal("done with neither agent nor task must fail")
	}
}

// relaysTo is the messages a box was given, in order.
func relaysTo(b *box) []string {
	var out []string
	for _, c := range b.snapshotCalls() {
		if strings.HasPrefix(c, "relay ") {
			out = append(out, c)
		}
	}
	return out
}

// An answer to an agent whose host does not answer is not lost, and the
// caller is not kept waiting on it: it is held at once, and the agent gets
// it — and what was held after it, in order — when its host is back.
func TestAFollowupToAnAgentOutOfReachIsHeldUntilItIsBack(t *testing.T) {
	f, boxes, out := newFleet(t)
	b := boxes["agent-03"]
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	st := f.State("agent-03")
	st.Task, st.Phase, st.UnreachableSince = "T1", reconcile.Waiting, t0
	f.setState(st)

	var held *Held
	if err := f.Followup(context.Background(), "T1", "operator", "ascending", t0.Add(time.Minute)); !errors.As(err, &held) || held.To != "agent-03" {
		t.Fatalf("err = %v; want it held", err)
	}
	if err := f.Followup(context.Background(), "T1", "operator", "and keep it stable", t0.Add(2*time.Minute)); !errors.As(err, &held) {
		t.Fatalf("second: %v", err)
	}
	if n := len(relaysTo(b)); n != 0 {
		t.Fatalf("tried a host that does not answer: %q", relaysTo(b))
	}
	th, _ := f.db.TaskThread("T1")
	if len(th) != 2 || th[0].Error != ledger.Held || th[1].Error != ledger.Held {
		t.Fatalf("thread = %+v", th)
	}

	b.obs = healthy("agent-03") // back
	f.Tick(context.Background(), t0.Add(10*time.Minute))
	got := relaysTo(b)
	if len(got) != 2 || !strings.Contains(got[0], "ascending") || !strings.Contains(got[1], "keep it stable") || !strings.Contains(got[0], "could not reach you") {
		t.Fatalf("relays = %q", got)
	}
	if th, _ := f.db.TaskThread("T1"); !th[0].Delivered || !th[1].Delivered {
		t.Fatalf("thread = %+v", th)
	}
	f.Tick(context.Background(), t0.Add(11*time.Minute))
	if n := len(relaysTo(b)); n != 2 {
		t.Fatalf("sent twice: %q", relaysTo(b))
	}
	if len(out.alerts) != 0 {
		t.Fatalf("alerts = %q", out.alerts)
	}
}

// A partition fleetd has not noticed yet: the relay hangs. The caller gets
// "queued" after holdAfter, not an error minutes later, and the message goes
// out once the relay works again.
func TestAFollowupWhoseRelayHangsIsHeld(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	f.holdAfter = 50 * time.Millisecond
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	b.relayHangs = true
	start := time.Now()
	var held *Held
	if err := f.Followup(context.Background(), "T1", "operator", "ascending", t0); !errors.As(err, &held) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s", d)
	}
	b.mu.Lock()
	b.relayHangs = false
	b.calls = nil
	b.mu.Unlock()
	f.Tick(context.Background(), t0.Add(time.Minute))
	if got := relaysTo(b); len(got) != 1 || !strings.Contains(got[0], "ascending") {
		t.Fatalf("relays = %q", got)
	}
}

// A message to a receiver with one still held waits behind it: they arrive in
// the order they were sent.
func TestAMessageWaitsBehindOneHeldForTheSameReceiver(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	b.relayErr = errors.New("relay to agent-03: not delivered: no session")
	var held *Held
	if err := f.Followup(context.Background(), "T1", "operator", "first", t0); !errors.As(err, &held) {
		t.Fatalf("first: %v", err)
	}
	b.relayErr = nil
	b.calls = nil
	if err := f.Followup(context.Background(), "T1", "operator", "second", t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := relaysTo(b); len(got) != 2 || !strings.Contains(got[0], "first") || !strings.Contains(got[1], "second") {
		t.Fatalf("relays = %q", got)
	}
}

// Cancelling the task of an agent out of reach closes it at once; the agent
// is not pressed (it cannot be), and its notice reaches it when it is back.
func TestACancelToAnAgentOutOfReachIsHeld(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	st := f.State("agent-03")
	st.Task, st.Phase, st.UnreachableSince = "T1", reconcile.Waiting, t0
	f.setState(st)
	var held *Held
	if err := f.Cancel(context.Background(), "T1", "operator", "not needed", t0.Add(time.Minute)); !errors.As(err, &held) {
		t.Fatalf("err = %v", err)
	}
	if tk, _ := f.db.Task("T1"); tk.Closed.IsZero() || tk.Outcome != "cancelled" {
		t.Fatalf("task = %+v", tk)
	}
	if calls := b.snapshotCalls(); len(calls) != 0 {
		t.Fatalf("calls = %q", calls)
	}
	b.obs = healthy("agent-03")
	f.Tick(context.Background(), t0.Add(5*time.Minute))
	if got := relaysTo(b); len(got) != 1 || !strings.Contains(got[0], "was cancelled") {
		t.Fatalf("relays = %q", got)
	}
}
