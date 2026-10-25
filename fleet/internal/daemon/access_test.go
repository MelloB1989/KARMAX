package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/tailnet"
)

// sandboxed makes agent-05 a sandbox agent.
func sandboxed(f *Fleet) {
	a := f.cfg.Agents["agent-05"]
	a.Sandbox = true
	f.cfg.Agents["agent-05"] = a
}

// A task is admin work only when the full scope gives it, for the operator's
// side, to an ordinary agent; and only admin work reads the operator's data.
func TestAdminWorkIsTheFullScopesTaskOnAnOrdinaryAgent(t *testing.T) {
	f, _, _ := newFleet(t)
	sandboxed(f)
	f.Tick(context.Background(), t0)
	ctx := context.Background()
	for _, c := range []struct {
		name string
		in   RunRequest
		want bool
	}{
		{"the full scope's run", RunRequest{Brief: "x", Agent: "agent-03", Task: "T-full", AdminWork: true}, true},
		{"a guest's run", RunRequest{Brief: "x", Agent: "agent-03", Task: "T-guest", From: "friend@example.com"}, false},
		{"the full scope's run on a sandbox", RunRequest{Brief: "x", Agent: "agent-05", Task: "T-sand", AdminWork: true}, false},
		{"the full scope's run for a login", RunRequest{Brief: "x", Agent: "agent-03", Task: "T-login", From: "friend@example.com", AdminWork: true}, false},
	} {
		if _, err := f.Run(ctx, c.in, t0); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		tk, err := f.db.Task(c.in.Task)
		if err != nil || tk.AdminWork != c.want {
			t.Errorf("%s: admin work %v, want %v (%v)", c.name, tk.AdminWork, c.want, err)
		}
		_ = f.Close(c.in.Agent, c.in.Task, "done", "", "", t0)
	}
	if err := f.Assign(ctx, "agent-05", "T-assign", "operator", "", t0); err != nil {
		t.Fatal(err)
	}
	if tk, _ := f.db.Task("T-assign"); tk.AdminWork {
		t.Error("an assignment to a sandbox is admin work")
	}
	putSecret(t, f, "openai", apiKey)
	if err := f.SecretGrant("openai", "", "agent-05", "operator", 0, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SecretRead("agent-05", "openai", t0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a sandbox on the operator's assignment read a secret: %v", err)
	}
}

// A guest's run goes only to its lent agents: picked among them, and a named
// agent outside them is refused.
func TestAGuestsRunGoesOnlyToItsLentAgents(t *testing.T) {
	f, _, _ := newFleet(t)
	f.Tick(context.Background(), t0)
	ctx := context.Background()
	rep, err := f.Run(ctx, RunRequest{Brief: "x", From: "friend@example.com", Among: []string{"agent-05"}}, t0)
	if err != nil || rep.Agent != "agent-05" {
		t.Fatalf("picked %s, %v", rep.Agent, err)
	}
	if _, err := f.Run(ctx, RunRequest{Brief: "x", From: "friend@example.com", Agent: "agent-03", Among: []string{"agent-05"}}, t0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a named agent outside the lends: %v", err)
	}
	var nf *NoFreeAgent
	if _, err := f.Run(ctx, RunRequest{Brief: "x", From: "friend@example.com", Among: []string{"agent-05"}}, t0); !errors.As(err, &nf) {
		t.Fatalf("its only agent busy: %v", err)
	}
	for _, c := range nf.Candidates {
		if c.Agent != "agent-05" {
			t.Errorf("a guest is told of %s", c.Agent)
		}
	}
	p := f.PickAmong("", []string{"agent-03"}, t0)
	if p.Agent != "agent-03" || len(p.Candidates) != 1 {
		t.Fatalf("PickAmong: %+v", p)
	}
}

func TestAKnockIsRecordedAndToldOnce(t *testing.T) {
	f, _, out := newFleet(t)
	who := tailnet.Who{StableID: "nF1", Login: "friend@example.com", Node: "friend-laptop.tail1234.ts.net"}
	for range 3 {
		if err := f.Knocked(who, t0); err != nil {
			t.Fatal(err)
		}
	}
	if f.KnockCount() != 1 {
		t.Fatalf("knocks: %d", f.KnockCount())
	}
	sandboxed(f)
	if _, err := f.ChangeAccess(fleetapi.AccessChange{Op: fleetapi.AccessLend, Device: "friend-laptop", Agents: []string{"agent-03"}}, "operator", t0); err == nil {
		t.Fatal("lent an ordinary agent")
	}
	if _, err := f.ChangeAccess(fleetapi.AccessChange{Op: fleetapi.AccessLend, Device: "friend-laptop", Agents: []string{"agent-05"}}, "operator", t0); err != nil {
		t.Fatal(err)
	}
	_, _ = f.ResolveDevice(who, t0)
	if d, err := f.ResolveDevice(who, t0); err != nil || d.Role != fleetapi.RoleGuest || !d.LastSeen.Equal(t0) {
		t.Fatalf("resolved: %+v %v", d, err)
	}
	if f.KnockCount() != 0 {
		t.Fatalf("knocks after the lend: %d", f.KnockCount())
	}
	evs, _ := f.Events(EventsQuery{Kinds: []string{"fleet.access.knock", "fleet.access.lend"}})
	if len(evs) != 2 {
		t.Fatalf("events: %+v", evs)
	}
	eventually(t, "the knock is told once", func() bool {
		out.mu.Lock()
		defer out.mu.Unlock()
		return len(out.alerts) == 1 && strings.Contains(out.alerts[0], "friend-laptop (friend@example.com) asked for fleet access")
	})
}

// A task that was admin work stops being so when its agent becomes a
// sandbox (fleet.yaml changed under it): the agent reads no secret.
func TestAnAgentMadeASandboxLosesItsAdminWork(t *testing.T) {
	f, _, _ := newFleet(t)
	f.Tick(context.Background(), t0)
	assignAs(t, f, "agent-05", "T1", "operator")
	putSecret(t, f, "openai", apiKey)
	if err := f.SecretGrant("openai", "", "agent-05", "operator", 0, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SecretRead("agent-05", "openai", t0); err != nil {
		t.Fatalf("an ordinary agent on admin work: %v", err)
	}
	sandboxed(f)
	if _, err := f.SecretRead("agent-05", "openai", t0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a sandbox read a secret: %v", err)
	}
}
