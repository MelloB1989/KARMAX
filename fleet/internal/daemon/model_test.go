package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/observe"
)

// live makes the status line of an agent's session show a model.
func live(b *box, name, id, display string) {
	st, _ := observe.ParseStatus([]byte(`{"session_id":"s-` + name + `","model":{"id":"` + id + `","display_name":"` + display + `"}}`))
	b.mu.Lock()
	b.obs.Status = st
	b.mu.Unlock()
}

// modelPrompts is the /model lines typed into a box.
func modelPrompts(b *box) []string {
	var out []string
	for _, c := range b.snapshotCalls() {
		if strings.HasPrefix(c, "prompt /model ") {
			out = append(out, strings.TrimPrefix(c, "prompt "))
		}
	}
	return out
}

func TestTheModelPromptIsSentOnlyWhenIdleAndDifferentAndOncePerChange(t *testing.T) {
	f, boxes, _ := newFleet(t)
	ctx := context.Background()
	b := boxes["agent-03"]
	live(b, "agent-03", "claude-sonnet-5-5", "Sonnet 5.5")

	f.Tick(ctx, t0)
	if got := modelPrompts(b); len(got) != 0 {
		t.Fatalf("nothing is wanted, yet %q", got)
	}
	if err := f.SetModel("agent-03", "Opus", t0); err != nil {
		t.Fatal(err)
	}

	b.mu.Lock()
	b.obs.Sessions[0].Status = "busy"
	b.mu.Unlock()
	f.Tick(ctx, t0.Add(time.Minute))
	if got := modelPrompts(b); len(got) != 0 {
		t.Fatalf("typed into a session mid-turn: %q", got)
	}

	b.mu.Lock()
	b.obs.Sessions[0].Status = "idle"
	b.mu.Unlock()
	f.Tick(ctx, t0.Add(2*time.Minute))
	f.Tick(ctx, t0.Add(3*time.Minute)) // the status line still shows sonnet: not asked again
	if got := modelPrompts(b); !slices.Equal(got, []string{"/model opus"}) {
		t.Fatalf("prompts = %q, want one /model opus", got)
	}

	live(b, "agent-03", "claude-opus-5-5", "Opus 5.5")
	f.Tick(ctx, t0.Add(4*time.Minute))
	if got := modelPrompts(b); len(got) != 1 {
		t.Fatalf("asked for the model it runs: %q", got)
	}

	if err := f.SetModel("agent-03", "sonnet", t0); err != nil {
		t.Fatal(err)
	}
	f.Tick(ctx, t0.Add(5*time.Minute))
	if got := modelPrompts(b); !slices.Equal(got, []string{"/model opus", "/model sonnet"}) {
		t.Fatalf("a change was not asked for: %q", got)
	}
}

func TestAnAskThatDoesNotTakeAlertsOnce(t *testing.T) {
	f, boxes, out := newFleet(t)
	ctx := context.Background()
	b := boxes["agent-03"]
	live(b, "agent-03", "claude-sonnet-5-5", "Sonnet 5.5")
	_ = f.SetModel("agent-03", "opus", t0)
	f.Tick(ctx, t0)
	f.Tick(ctx, t0.Add(modelAskGrace+time.Second))
	f.Tick(ctx, t0.Add(modelAskGrace+time.Minute))
	if len(out.alerts) != 1 || !strings.Contains(out.alerts[0], "still shows") {
		t.Fatalf("alerts = %q", out.alerts)
	}
}

func TestAModelOverrideSurvivesAFleetdRestart(t *testing.T) {
	f, boxes, out := newFleet(t)
	ctx := context.Background()
	if err := f.SetModel("agent-03", "claude-opus-5-5", t0); err != nil {
		t.Fatal(err)
	}
	if err := f.SetModel("agent-05", "haiku", t0); err != nil {
		t.Fatal(err)
	}
	if err := f.SetModel("agent-05", "", t0); err != nil { // reset
		t.Fatal(err)
	}

	f2, err := New(f.cfg, f.db, func(name string) Box { return boxes[name] }, out, out)
	if err != nil {
		t.Fatal(err)
	}
	f2.soonEvery = 0
	if r, _ := f2.Model("agent-03"); r.Override != "claude-opus-5-5" || r.Wanted != "claude-opus-5-5" {
		t.Errorf("override after restart = %+v", r)
	}
	if r, _ := f2.Model("agent-05"); r.Override != "" {
		t.Errorf("a reset override came back: %+v", r)
	}
	live(boxes["agent-03"], "agent-03", "claude-sonnet-5-5", "Sonnet 5.5")
	f2.Tick(ctx, t0)
	if got := modelPrompts(boxes["agent-03"]); !slices.Equal(got, []string{"/model claude-opus-5-5"}) {
		t.Errorf("prompts = %q", got)
	}
}

func TestRunWithAModelOnlyConsidersAgentsThatListIt(t *testing.T) {
	f, boxes, _ := newFleet(t)
	ctx := context.Background()
	f.Tick(ctx, t0) // learns each agent's session
	rep, err := f.Run(ctx, RunRequest{Brief: "do a thing", Model: "opus"}, t0)
	if err != nil || rep.Agent != "agent-03" {
		t.Fatalf("run = %+v, %v; want agent-03, the only agent listing opus", rep, err)
	}
	if got := modelPrompts(boxes["agent-03"]); !slices.Equal(got, []string{"/model opus"}) {
		t.Errorf("the session was not asked for the model before the brief: %q", got)
	}
	// The brief goes in after /model.
	calls := boxes["agent-03"].snapshotCalls()
	if len(calls) < 2 || calls[0] != "prompt /model opus" || !strings.HasPrefix(calls[1], "relay") {
		t.Errorf("calls = %q", calls)
	}
	if task, _ := f.db.Task(rep.Task); task.Model != "opus" {
		t.Errorf("task model = %q", task.Model)
	}

	// With agent-03 taken, nobody else lists opus, and the 409 says why.
	_, err = f.Run(ctx, RunRequest{Brief: "another thing", Model: "opus"}, t0)
	var nf *NoFreeAgent
	if !errors.As(err, &nf) || !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want no free agent", err)
	}
	for _, c := range nf.Candidates {
		if c.Agent == "agent-05" && !strings.Contains(c.Why, "does not list opus") {
			t.Errorf("agent-05: %q", c.Why)
		}
	}
	// A named agent that does not list it is refused as well.
	if _, err := f.Run(ctx, RunRequest{Brief: "x", Model: "opus", Agent: "agent-05"}, t0); !errors.Is(err, ErrConflict) {
		t.Errorf("named agent not listing opus: %v", err)
	}
	if _, err := f.Run(ctx, RunRequest{Brief: "x", Model: "opus\n/exit"}, t0); !errors.Is(err, ErrBadInput) {
		t.Errorf("a model with a newline: %v", err)
	}
}

func TestTheAgentGoesBackToItsOverrideWhenTheTaskSessionRotates(t *testing.T) {
	f, boxes, _ := newFleet(t)
	ctx := context.Background()
	b := boxes["agent-03"]
	live(b, "agent-03", "claude-haiku-5-5", "Haiku 5.5")
	if err := f.SetModel("agent-03", "haiku", t0); err != nil {
		t.Fatal(err)
	}
	f.Tick(ctx, t0) // learns the session
	rep, err := f.Run(ctx, RunRequest{Brief: "do a thing", Model: "opus", Agent: "agent-03"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := f.Model("agent-03"); r.Wanted != "opus" || r.Task != "opus" || r.Override != "haiku" {
		t.Fatalf("while the task is open: %+v", r)
	}
	live(b, "agent-03", "claude-opus-5-5", "Opus 5.5")
	f.Tick(ctx, t0.Add(time.Minute))
	if got := modelPrompts(b); !slices.Equal(got, []string{"/model opus"}) {
		t.Fatalf("prompts = %q", got)
	}

	if err := f.Done("agent-03", rep.Task, "done", "", "", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.Tick(ctx, t0.Add(2*time.Minute)) // rotates
	if !slices.Contains(b.snapshotCalls(), "fresh") {
		t.Fatalf("not rotated: %q", b.snapshotCalls())
	}
	if r, _ := f.Model("agent-03"); r.Wanted != "haiku" || r.Task != "" {
		t.Fatalf("after the task closed: %+v", r)
	}
	// The fresh session starts on the CLI's default, and is put back.
	live(b, "agent-03", "claude-sonnet-5-5", "Sonnet 5.5")
	f.Tick(ctx, t0.Add(3*time.Minute))
	if got := modelPrompts(b); !slices.Equal(got, []string{"/model opus", "/model haiku"}) {
		t.Errorf("prompts = %q, want the agent put back on haiku", got)
	}
}

func TestModelAllowedMatchesAliasesAndIds(t *testing.T) {
	for _, c := range []struct {
		list  []string
		model string
		want  bool
	}{
		{[]string{"sonnet"}, "sonnet", true},
		{[]string{"sonnet"}, "opus", false},
		{[]string{"sonnet", "opus"}, "claude-opus-5-5", true},
		{[]string{"claude-opus-5-5"}, "opus", true},
		{[]string{"claude-opus-5-5"}, "claude-opus-5-5", true},
		{[]string{"claude-opus-5-5"}, "claude-opus-4-1", false},
		{[]string{"sonnet"}, "claude-haiku-5-5", false},
	} {
		if got := ModelAllowed(c.list, c.model); got != c.want {
			t.Errorf("ModelAllowed(%v, %q) = %v", c.list, c.model, got)
		}
	}
}

func TestTheModelLedgerKeepsAnOverrideAndATaskModel(t *testing.T) {
	f, _, _ := newFleet(t)
	if err := f.db.OpenTask("T1", "agent-03", "operator", t0); err != nil {
		t.Fatal(err)
	}
	_ = f.db.SetTaskModel("T1", "opus")
	tasks, _ := f.db.Tasks(true, "agent-03", 0)
	if len(tasks) != 1 || tasks[0].Model != "opus" {
		t.Errorf("tasks = %+v", tasks)
	}
	var _ ledger.Task = tasks[0]
}
