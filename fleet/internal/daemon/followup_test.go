package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
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
