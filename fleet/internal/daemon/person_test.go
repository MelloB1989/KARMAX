package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

const ram = "ram@example.com"

// Someone on the tailnet runs a task as themselves: the agent is told who
// asked, reports to them, and the report is filed for them to read.
func TestAPersonOnTheTailnetRunsATaskAndReadsItsReport(t *testing.T) {
	f, boxes := newPool(t)
	ctx := context.Background()
	if _, err := f.Run(ctx, RunRequest{Brief: "fix it", Agent: "agent-01", From: ram, Task: "T1", Limit: 2}, t0); err != nil {
		t.Fatal(err)
	}
	if got := boxes["agent-01"].calls; len(got) != 1 || got[0] != "relay "+ram+"→agent-01: Task T1 from "+ram+". fix it" {
		t.Fatalf("calls = %q", got)
	}
	if err := f.Tell(ctx, "agent-01", ram, "DONE T1: branch fleet/agent-01/fix", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for n, b := range boxes {
		if n != "agent-01" && len(b.calls) > 0 {
			t.Fatalf("%s was sent %q: a person's report is filed, not relayed", n, b.calls)
		}
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	w, err := f.Wait(wctx, "", "T1", 0)
	if err != nil || w.Reason != "message" || w.Message.From != "agent-01" || w.Message.To != ram || !strings.HasPrefix(w.Message.Text, "DONE T1") {
		t.Fatalf("wait = %+v, %v", w, err)
	}
	if err := f.Followup(ctx, "T1", ram, "add a test", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := boxes["agent-01"].calls; len(got) != 2 || !strings.Contains(got[1], "follow-up from "+ram+". add a test") {
		t.Fatalf("calls = %q", got)
	}
}

// One person cannot take the whole fleet: past their limit a run is
// refused, whoever else is free, until one of theirs closes.
func TestAPersonHoldsAtMostTheirLimitOfOpenTasks(t *testing.T) {
	f, _ := newPool(t)
	ctx := context.Background()
	for _, id := range []string{"T1", "T2"} {
		if _, err := f.Run(ctx, RunRequest{Brief: "work", From: ram, Task: id, Limit: 2}, t0); err != nil {
			t.Fatal(err)
		}
	}
	_, err := f.Run(ctx, RunRequest{Brief: "more", From: ram, Task: "T3", Limit: 2}, t0)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "holds 2 open tasks") || !strings.Contains(err.Error(), "T1, T2") &&
		!strings.Contains(err.Error(), "T2, T1") {
		t.Fatalf("third run: %v", err)
	}
	if _, err := f.db.Task("T3"); err == nil {
		t.Fatal("a refused run left a task")
	}
	// The operator has no limit, and someone else's tasks are not counted.
	if open, _ := f.db.TasksBy(true, "", "operator", 0); len(open) != 0 {
		t.Fatalf("operator's = %+v", open)
	}
	if err := f.db.CloseTask("T1", t0.Add(time.Minute), "done", "", ""); err != nil {
		t.Fatal(err)
	}
	rep, err := f.Run(ctx, RunRequest{Brief: "more", From: ram, Task: "T3", Limit: 2}, t0.Add(2*time.Minute))
	if err != nil || !slices.Contains([]string{"agent-01", "agent-02", "agent-03"}, rep.Agent) {
		t.Fatalf("after one closed: %+v, %v", rep, err)
	}
}

// People read the ledger; the fleet does not carry messages between them.
func TestPeopleDoNotMessageEachOtherThroughTheFleet(t *testing.T) {
	f, _ := newPool(t)
	for _, c := range [][2]string{{ram, "operator"}, {"operator", ram}, {ram, "x@example.com"}} {
		if err := f.Tell(context.Background(), c[0], c[1], "hi", t0); err == nil {
			t.Errorf("%s → %s went", c[0], c[1])
		}
	}
}
