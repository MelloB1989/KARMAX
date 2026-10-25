package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// A guest's question is the guest's to answer: after the window it is told
// to them once more, never to the orchestrator, and it never pages the
// operator.
func TestAGuestsQuestionDoesNotEscalateToTheOrchestrator(t *testing.T) {
	for _, from := range []string{ram, ram + "/laptop"} {
		t.Run(from, func(t *testing.T) {
			f, boxes := newPool(t)
			o := &outbox{}
			f.notify = o
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if _, err := f.Run(ctx, RunRequest{Brief: "work", Agent: "agent-01", From: from, Task: "T1"}, t0); err != nil {
				t.Fatal(err)
			}
			askOn(t, f, ctx, "agent-01", "T1", "which file?", time.Hour, t0)
			questions := func() (n int, last ledger.Relay) {
				th, _ := f.db.TaskThread("T1")
				for _, m := range th {
					if m.Kind == ledger.KindQuestion {
						n, last = n+1, m
					}
				}
				return n, last
			}
			window := f.cfg.Thresholds.QuestionEscalate
			before := len(boxes["karmax"].snapshotCalls())
			if f.escalate(ctx, t0.Add(window-time.Second)); func() int { n, _ := questions(); return n }() != 1 {
				t.Fatal("told again before the window")
			}
			f.escalate(ctx, t0.Add(window))
			n, last := questions()
			if n != 2 || last.To != from || last.From != "agent-01" || !strings.Contains(last.Text, "which file?") || !strings.Contains(last.Text, "fleetctl answer T1") {
				t.Fatalf("after the window: %d questions, last %+v", n, last)
			}
			for k := 2; k <= 6; k++ {
				f.escalate(ctx, t0.Add(time.Duration(k)*window))
			}
			if n, _ := questions(); n != 2 {
				t.Errorf("told %d times, want once more", n-1)
			}
			if got := len(boxes["karmax"].snapshotCalls()); got != before {
				t.Errorf("the orchestrator was sent %d messages", got-before)
			}
			if len(o.alerts) != 0 {
				t.Errorf("alerts = %q", o.alerts)
			}
			// It is still a question the agent is blocked on.
			if asking, _, _ := f.db.Asking("T1"); !asking {
				t.Error("the re-tell ended the question")
			}
		})
	}
}

// newSandboxPool is newPool with agent-03 a sandbox.
func newSandboxPool(t *testing.T) *Fleet {
	t.Helper()
	f, _ := newPool(t)
	a := f.cfg.Agents["agent-03"]
	a.Sandbox = true
	f.cfg.Agents["agent-03"] = a
	return f
}

// A sandbox is lent to guests: a pick for anyone else passes it over, and it
// takes an admin's task only when the admin names it.
func TestAPickSkipsSandboxesForAdminsUnlessNamed(t *testing.T) {
	ctx := context.Background()
	f := newSandboxPool(t)
	for _, n := range []string{"agent-01", "agent-02"} {
		set(f, n, func(s *reconcile.State) { s.Phase = reconcile.Down })
	}
	p := f.Pick("", t0)
	if p.Agent != "" {
		t.Fatalf("picked %s", p.Agent)
	}
	for _, c := range p.Candidates {
		if c.Agent == "agent-03" && (c.Eligible || !strings.Contains(c.Why, "sandbox")) {
			t.Errorf("agent-03 = %+v", c)
		}
	}
	if _, err := f.Run(ctx, RunRequest{Brief: "work", From: "operator"}, t0); err == nil {
		t.Fatal("an admin's run went to a sandbox it did not name")
	}
	rep, err := f.Run(ctx, RunRequest{Brief: "work", From: "operator", Agent: "agent-03", Task: "T1"}, t0)
	if err != nil || rep.Agent != "agent-03" {
		t.Fatalf("named: %+v, %v", rep, err)
	}

	// A guest it is lent to gets it from a pick.
	f = newSandboxPool(t)
	set(f, "agent-01", func(s *reconcile.State) { s.Phase = reconcile.Down })
	set(f, "agent-02", func(s *reconcile.State) { s.Phase = reconcile.Down })
	rep, err = f.Run(ctx, RunRequest{Brief: "work", From: ram, Among: []string{"agent-03"}, Task: "T2"}, t0)
	if err != nil || rep.Agent != "agent-03" {
		t.Fatalf("a guest's run: %+v, %v", rep, err)
	}
	// One it is not lent to does not.
	f = newSandboxPool(t)
	set(f, "agent-01", func(s *reconcile.State) { s.Phase = reconcile.Down })
	set(f, "agent-02", func(s *reconcile.State) { s.Phase = reconcile.Down })
	if rep, err := f.Run(ctx, RunRequest{Brief: "work", From: ram, Among: []string{"agent-02"}, Task: "T3"}, t0); err == nil {
		t.Fatalf("a guest got a sandbox not lent to it: %+v", rep)
	}
}
