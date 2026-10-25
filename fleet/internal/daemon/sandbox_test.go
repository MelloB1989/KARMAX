package daemon

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
	"github.com/MelloB1989/karmax/fleet/internal/target"
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

// probeBox is a sandbox's container whose network probe passes or fails.
type probeBox struct {
	*box
	mu    sync.Mutex
	fails bool
	argvs [][]string
}

func (p *probeBox) Rsh(_ context.Context, argv []string, _ io.Reader, _ io.Writer) (target.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.argvs = append(p.argvs, argv)
	if p.fails {
		return target.Result{Exit: 1, Stderr: "192.0.2.10:22 answers;"}, nil
	}
	return target.Result{}, nil
}

func (p *probeBox) set(fails bool) { p.mu.Lock(); p.fails = fails; p.mu.Unlock() }

func (p *probeBox) probes() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.argvs) }

// newProbed is a pool with agent-03 a sandbox on pc2, whose fleetd_url names
// fleetd's address, and the probe box agent-03's container answers with.
func newProbed(t *testing.T) (*Fleet, *probeBox, *outbox) {
	t.Helper()
	f, boxes := newPool(t)
	a := f.cfg.Agents["agent-03"]
	a.Sandbox = true
	f.cfg.Agents["agent-03"] = a
	h := f.cfg.Hosts["pc2"]
	h.FleetdURL = "http://192.0.2.10:7879"
	f.cfg.Hosts["pc2"] = h
	p := &probeBox{box: boxes["agent-03"]}
	inner := f.box
	f.box = func(name string) Box {
		if name == "agent-03" {
			return p
		}
		return inner(name)
	}
	o := &outbox{}
	f.notify = o
	return f, p, o
}

func TestAnUnguardedSandboxIsDisabledAndToldOnce(t *testing.T) {
	ctx := context.Background()
	f, p, o := newProbed(t)
	p.set(true)
	f.probeSandboxes(ctx, t0)
	if st := f.State("agent-03"); st.Phase != reconcile.Disabled || st.DisabledReason != ReasonUnguarded {
		t.Fatalf("agent-03 = %s (%s), want disabled: %s", st.Phase, st.DisabledReason, ReasonUnguarded)
	}
	if len(o.alerts) != 1 || !strings.Contains(o.alerts[0], "agent-03") || !strings.Contains(o.alerts[0], "192.0.2.10:22") ||
		!strings.Contains(o.alerts[0], "fleet-sandbox-net") {
		t.Fatalf("alerts = %q", o.alerts)
	}
	argv := strings.Join(p.argvs[0], " ")
	if !strings.Contains(argv, "host.docker.internal:22") || !strings.Contains(argv, "192.0.2.10:22") || !strings.Contains(argv, "/healthz") {
		t.Errorf("probe = %s", argv)
	}
	// Within the hour it is not probed again; an hour on, still unguarded, it
	// is not told again.
	f.probeSandboxes(ctx, t0.Add(30*time.Minute))
	if p.probes() != 1 {
		t.Fatalf("probed %d times within the hour", p.probes())
	}
	f.probeSandboxes(ctx, t0.Add(time.Hour))
	if p.probes() != 2 || len(o.alerts) != 1 {
		t.Fatalf("probes %d, alerts %q", p.probes(), o.alerts)
	}
	// A pick passes it over, even for a guest it is lent to.
	if r := f.pickAmong("", ram, "", []string{"agent-03"}, t0.Add(time.Hour)); r.Agent == "agent-03" {
		t.Fatal("an unguarded sandbox was picked")
	}
	// Ordinary agents are not probed.
	for _, n := range []string{"agent-01", "agent-02"} {
		if st := f.State(n); st.Phase == reconcile.Disabled {
			t.Errorf("%s disabled", n)
		}
	}
}

func TestAGuardedSandboxComesBack(t *testing.T) {
	ctx := context.Background()
	f, p, _ := newProbed(t)
	p.set(true)
	f.probeSandboxes(ctx, t0)
	p.set(false)
	f.probeSandboxes(ctx, t0.Add(time.Hour))
	if st := f.State("agent-03"); st.Phase == reconcile.Disabled {
		t.Fatalf("agent-03 still disabled (%s) after its probe passed", st.DisabledReason)
	}
	// Disabled for its subscription, a passing probe does not clear it.
	set(f, "agent-03", func(s *reconcile.State) { s.Phase, s.DisabledReason = reconcile.Disabled, "billing_error" })
	f.probeSandboxes(ctx, t0.Add(2*time.Hour))
	if st := f.State("agent-03"); st.Phase != reconcile.Disabled || st.DisabledReason != "billing_error" {
		t.Fatalf("agent-03 = %s (%s)", st.Phase, st.DisabledReason)
	}
}

// A probe that cannot run (the container is down) is no verdict.
func TestAProbeThatCannotRunChangesNothing(t *testing.T) {
	ctx := context.Background()
	f := newSandboxPool(t)
	o := &outbox{}
	f.notify = o
	f.probeSandboxes(ctx, t0) // the plain box's Rsh fails
	if st := f.State("agent-03"); st.Phase == reconcile.Disabled || len(o.alerts) != 0 {
		t.Fatalf("agent-03 = %s, alerts %q", st.Phase, o.alerts)
	}
}
