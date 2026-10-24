package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/agent"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

func relayCount(b *box) (n int) {
	for _, c := range b.snapshotCalls() {
		if strings.HasPrefix(c, "relay ") {
			n++
		}
	}
	return n
}

// A message a receiver could not take stays until it can, however long that
// is: not dropped after a few tries or a day. It backs off meanwhile, you are
// told once it has waited long — once, across fleetd restarts — and it is
// delivered when the receiver is back.
func TestAHeldMessageOutlivesTheOldWindowsAndIsDeliveredWhenTheReceiverReturns(t *testing.T) {
	f, boxes, out := newFleet(t)
	b := boxes["agent-03"]
	b.relayErr = errors.New("no session named agent-03")
	_ = f.db.OpenTask("T1", "agent-03", "karmax", t0)
	_, _ = f.db.BeginRelay(ledger.Relay{From: "karmax", To: "agent-03", Task: "T1", Text: "the brief", At: t0})
	f, err := New(f.cfg, f.db, func(name string) Box { return boxes[name] }, out, out)
	if err != nil {
		t.Fatal(err)
	}
	ticks := 0
	for m := 1; m <= 120; m++ {
		f.Tick(context.Background(), t0.Add(time.Duration(m)*time.Minute))
		ticks++
	}
	if n := relayCount(b); n < 2 || n >= ticks/2 {
		t.Fatalf("tried %d times in %d ticks: want a growing pause between tries", n, ticks)
	}
	if len(out.alerts) != 1 || !strings.Contains(out.alerts[0], "T1") || !strings.Contains(out.alerts[0], "held") {
		t.Fatalf("alerts = %q: want one, for the long hold", out.alerts)
	}
	// A fleetd restarted later does not say it again, and still holds it.
	f, err = New(f.cfg, f.db, func(name string) Box { return boxes[name] }, out, out)
	if err != nil {
		t.Fatal(err)
	}
	f.Tick(context.Background(), t0.Add(5*time.Hour))
	if len(out.alerts) != 1 {
		t.Fatalf("alerts = %q", out.alerts)
	}
	th, _ := f.db.TaskThread("T1")
	if len(th) != 1 || th[0].Delivered || !th[0].Held {
		t.Fatalf("thread = %+v: want it held, not dropped", th)
	}
	// The receiver is back, a day and a half on.
	b.relayErr = nil
	f.Tick(context.Background(), t0.Add(36*time.Hour))
	th, _ = f.db.TaskThread("T1")
	if len(th) != 1 || !th[0].Delivered || th[0].Held {
		t.Fatalf("thread = %+v: want it delivered", th)
	}
	if !slices.ContainsFunc(b.snapshotCalls(), func(c string) bool { return strings.Contains(c, "the brief") }) {
		t.Fatalf("calls = %q", b.snapshotCalls())
	}
}

// Tries are not burned on a receiver that is known to be down.
func TestAMessageForADownReceiverIsNotTriedAndNotDropped(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	_ = f.db.OpenTask("T1", "agent-03", "karmax", t0)
	id, _ := f.db.BeginRelay(ledger.Relay{From: "karmax", To: "agent-03", Task: "T1", Text: "the brief", At: t0})
	_ = f.db.HoldRelay(id)
	b.obs.ContainerUp = false
	for m := 1; m <= 30; m++ {
		f.Tick(context.Background(), t0.Add(time.Duration(m)*time.Minute))
	}
	if n := relayCount(b); n != 0 {
		t.Fatalf("tried %d times a receiver that is down", n)
	}
	p, _ := f.db.Pending("agent-03", time.Time{})
	if len(p) != 1 {
		t.Fatalf("pending = %+v", p)
	}
}

// What an agent queued while fleetd was away is delivered whenever it is
// found, however old, and what cannot go yet is held, not dropped.
func TestAnOutboxItemOlderThanTheOldWindowsIsDeliveredWhenTheReceiverReturns(t *testing.T) {
	f, boxes, out := newFleet(t)
	b := boxes["agent-03"]
	boxes["karmax"].relayErr = errors.New("relay to karmax: not delivered: no session")
	b.obs.Outbox = []reconcile.Outboxed{{File: "1-1.json", From: "agent-03", To: "karmax", Text: "DONE T9", At: t0.Add(-30 * time.Hour)}}
	f.Tick(context.Background(), t0)
	if !slices.Contains(b.snapshotCalls(), "ackoutbox 1-1.json") {
		t.Fatalf("calls = %q: want the item taken into the ledger", b.snapshotCalls())
	}
	if len(out.alerts) != 1 || !strings.Contains(out.alerts[0], "held") {
		t.Fatalf("alerts = %q: want one for a message held 30h", out.alerts)
	}
	boxes["karmax"].relayErr = nil
	b.obs.Outbox = nil
	f.Tick(context.Background(), t0.Add(10*time.Minute))
	if !slices.ContainsFunc(boxes["karmax"].snapshotCalls(), func(c string) bool { return strings.Contains(c, "DONE T9") }) {
		t.Fatalf("karmax calls %q", boxes["karmax"].snapshotCalls())
	}
	if p, _ := f.db.Pending("karmax", time.Time{}); len(p) != 0 {
		t.Fatalf("still pending: %+v", p)
	}
}

// A held message goes when its task closes, not before: closing the task
// ends the wait.
func TestAHeldMessageEndsWhenItsTaskCloses(t *testing.T) {
	f, _, _ := newFleet(t)
	_ = f.db.OpenTask("T1", "agent-03", "karmax", t0)
	id, _ := f.db.BeginRelay(ledger.Relay{From: "agent-03", To: "karmax", Task: "T1", Text: "report", At: t0})
	_ = f.db.HoldRelay(id)
	if p, _ := f.db.Pending("karmax", time.Time{}); len(p) != 1 {
		t.Fatalf("pending = %+v", p)
	}
	_ = f.db.CloseTask("T1", t0.Add(time.Hour), "done", "", "")
	if p, _ := f.db.Pending("karmax", time.Time{}); len(p) != 0 {
		t.Fatalf("pending after the task closed = %+v", p)
	}
}

// The carry-on prompt of a resumed session says what it was doing, what
// reaches it next, what its worktrees look like, and that its background
// commands died.
func TestTheResumePromptCarriesTheTaskItsMessagesAndItsWorktrees(t *testing.T) {
	f, boxes, _ := newFleet(t)
	b := boxes["agent-03"]
	_ = f.db.OpenTask("T1", "agent-03", "karmax", t0)
	_ = f.db.AddRelay(ledger.Relay{From: "karmax", To: "agent-03", Task: "T1", Text: "Task T1 from karmax.\nAdd the thing.\nThen test it.", At: t0, Delivered: true})
	id, _ := f.db.BeginRelay(ledger.Relay{From: "karmax", To: "agent-03", Task: "T1", Text: "also mind the docs", At: t0.Add(time.Minute)})
	_ = f.db.HoldRelay(id)
	b.wts = []agent.Worktree{{Path: "/work/wt/thing", Branch: "fleet/agent-03/thing", Dirty: 2, Unpushed: 1, Status: " M a.go\n?? b.go"}}
	st := f.State("agent-03")
	st.Task, st.Phase, st.SessionID = "T1", reconcile.Working, "s-agent-03"
	st.TurnOpen, st.TurnProc, st.Proc = true, "boot-1/7", "boot-1/7"
	f.setState(st)
	b.obs.Proc = "boot-2/7"
	f.Tick(context.Background(), t0.Add(time.Hour))
	var prompt string
	for _, c := range b.snapshotCalls() {
		if strings.HasPrefix(c, "prompt ") {
			prompt = c
		}
	}
	for _, want := range []string{"T1", "Add the thing.", "also mind the docs", "/work/wt/thing", "fleet/agent-03/thing", "?? b.go", "1 commit(s) not pushed", "Background commands", "killed"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the resume prompt lacks %q:\n%s", want, prompt)
		}
	}
}
