package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/memory"
)

type recaller struct{ asked []string }

func (r *recaller) Recall(_ context.Context, q string, limit int) (*memory.Result, error) {
	r.asked = append(r.asked, q)
	return &memory.Result{Query: q, Memories: []memory.Memory{{Path: "people/ram.md", Content: "Ram runs the build host."}}}, nil
}

func TestRecallAsksTheMemoryAndIsOffWithoutOne(t *testing.T) {
	f, _, _ := newFleet(t)
	if _, err := f.Recall(context.Background(), "ram", 5); !errors.Is(err, ErrMemoryOff) {
		t.Fatalf("no memory configured: err = %v", err)
	}
	r := &recaller{}
	f.Memory = r
	res, err := f.Recall(context.Background(), "ram", 5)
	if err != nil || len(res.Memories) != 1 || len(r.asked) != 1 || r.asked[0] != "ram" {
		t.Fatalf("res = %+v, %v; asked %q", res, err, r.asked)
	}
}

// A suggestion goes to the orchestrator, which saves it or not with its own
// tools: fleetd never writes memory.
func TestASuggestionReachesTheOrchestratorWithWhoAndTheTask(t *testing.T) {
	f, boxes, _ := newFleet(t)
	f.Memory = &recaller{}
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	to, err := f.Suggest(context.Background(), "agent-03", "Ram prefers squash merges", "he said so in review", t0)
	if err != nil || to != "karmax" {
		t.Fatalf("to = %q, %v", to, err)
	}
	got := boxes["karmax"].calls
	if len(got) != 1 {
		t.Fatalf("orchestrator relay calls = %q", got)
	}
	for _, want := range []string{"agent-03→karmax", "Memory suggestion", "task T1", "Ram prefers squash merges", "he said so in review"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("relay %q lacks %q", got[0], want)
		}
	}
	th, _ := f.db.TaskThread("T1")
	if len(th) == 0 || !strings.Contains(th[len(th)-1].Text, "Ram prefers squash merges") {
		t.Errorf("the suggestion is not filed under the task: %+v", th)
	}
	if _, err := f.Suggest(context.Background(), "agent-03", "  ", "", t0); !errors.Is(err, ErrBadInput) {
		t.Errorf("an empty suggestion: %v", err)
	}
}

func TestOperatorSideAssignersAreTheOperatorSessionsAndTailnetOperators(t *testing.T) {
	f, _, _ := newFleet(t)
	f.cfg.Tailnet = &config.Tailnet{Operators: []string{"op@example.com"}, Members: []string{"ram@example.com"}}
	for assigner, want := range map[string]bool{
		"operator": true, "karmax": true, "relay-from-operator": true, "op@example.com": true,
		"ram@example.com": false, "sam@example.com": false, "": false,
	} {
		if got := f.OperatorAssigner(assigner); got != want {
			t.Errorf("OperatorAssigner(%q) = %v, want %v", assigner, got, want)
		}
	}
}
