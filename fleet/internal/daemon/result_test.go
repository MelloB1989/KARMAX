package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReportAttachesToTask(t *testing.T) {
	f, _, _ := newFleet(t)
	ctx := context.Background()
	_ = f.Assign(ctx, "agent-03", "T1", "operator", "", t0)
	if err := f.Tell(ctx, "agent-03", "operator", "ready", t0); err != nil {
		t.Fatal(err)
	}
	if err := f.Tell(ctx, "agent-05", "operator", "unrelated", t0); err != nil {
		t.Fatal(err)
	}
	got, _ := f.db.TaskReports("T1", 0)
	if len(got) != 1 || got[0].Text != "ready" || got[0].Task != "T1" {
		t.Fatalf("reports = %+v", got)
	}
}

func TestRelayAttachesToTask(t *testing.T) {
	f, boxes, _ := newFleet(t)
	ctx := context.Background()
	_ = f.Assign(ctx, "agent-03", "T1", "karmax", "", t0)
	if err := f.Tell(ctx, "agent-03", "karmax", "pr is up", t0); err != nil {
		t.Fatal(err)
	}
	if len(boxes["karmax"].calls) != 1 {
		t.Fatalf("not relayed: %v", boxes["karmax"].calls)
	}
	got, _ := f.db.TaskReports("T1", 0)
	if len(got) != 1 || got[0].To != "karmax" || got[0].Text != "pr is up" {
		t.Fatalf("reports = %+v", got)
	}
}

// The orchestrator's messages to an agent are not the agent's reports.
func TestOnlyAnAgentsOwnMessagesAttach(t *testing.T) {
	f, _, _ := newFleet(t)
	ctx := context.Background()
	_ = f.Assign(ctx, "agent-03", "T1", "karmax", "", t0)
	_ = f.Tell(ctx, "karmax", "agent-03", "more please", t0)
	if got, _ := f.db.TaskReports("T1", 0); len(got) != 0 {
		t.Fatalf("reports = %+v", got)
	}
}

func TestReportAfterDoneAttaches(t *testing.T) {
	f, _, _ := newFleet(t)
	ctx := context.Background()
	_ = f.Assign(ctx, "agent-03", "T1", "operator", "", t0)
	_ = f.Done("agent-03", "T1", "done", "fleet/agent-03/x", "", t0)
	_ = f.Tell(ctx, "agent-03", "operator", "one more thing", t0)
	got, _ := f.db.TaskReports("T1", 0)
	if len(got) != 1 {
		t.Fatalf("reports = %+v", got)
	}
}

func TestWaitByTaskSeesReportToOrchestrator(t *testing.T) {
	f, _, _ := newFleet(t)
	f.waitEvery = 10 * time.Millisecond
	ctx := context.Background()
	_ = f.Assign(ctx, "agent-03", "T1", "karmax", "", t0)
	done := waitAsync(f, ctx, "agent-03", "T1", 0)
	time.Sleep(30 * time.Millisecond)
	_ = f.Tell(ctx, "agent-03", "karmax", "finished", t0)
	r := await(t, done)
	if r.Reason != "message" || r.Message == nil || r.Message.Text != "finished" || r.Message.To != "karmax" {
		t.Fatalf("wait = %+v", r)
	}
}

func TestWaitByTaskIgnoresOtherTasksReports(t *testing.T) {
	f, _, _ := newFleet(t)
	f.waitEvery = 10 * time.Millisecond
	ctx := context.Background()
	_ = f.Assign(ctx, "agent-03", "T1", "operator", "", t0)
	_ = f.Assign(ctx, "agent-05", "T2", "operator", "", t0)
	done := waitAsync(f, ctx, "", "T1", 0)
	time.Sleep(30 * time.Millisecond)
	_ = f.Tell(ctx, "agent-05", "operator", "mine, not yours", t0)
	_ = f.Tell(ctx, "agent-03", "operator", "yours", t0)
	if r := await(t, done); r.Message == nil || r.Message.Text != "yours" {
		t.Fatalf("wait = %+v", r)
	}
}

func TestWaitByTaskNoAgent(t *testing.T) {
	f, _, _ := newFleet(t)
	f.waitEvery = 10 * time.Millisecond
	ctx := context.Background()
	if _, err := f.Wait(ctx, "", "T404", 0); !errors.Is(err, ErrNoTask) {
		t.Fatalf("unknown task: err = %v", err)
	}
	if _, err := f.Wait(ctx, "", "", 0); err == nil {
		t.Fatal("wait with neither agent nor task should fail")
	}
	_ = f.Assign(ctx, "agent-03", "T1", "operator", "", t0)
	done := waitAsync(f, ctx, "", "T1", 0)
	time.Sleep(30 * time.Millisecond)
	_ = f.Done("agent-03", "T1", "done", "", "", t0)
	if r := await(t, done); r.Reason != "closed" {
		t.Fatalf("wait = %+v", r)
	}
}

func TestTaskAgent(t *testing.T) {
	f, _, _ := newFleet(t)
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	tk, err := f.TaskAgent("T1")
	if err != nil || tk.Agent != "agent-03" {
		t.Fatalf("task = %+v, %v", tk, err)
	}
	if _, err := f.TaskAgent("T404"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("err = %v", err)
	}
}

type gitCall struct {
	dir  string
	args []string
}

func fakeGit(calls *[]gitCall, out string, err error) func(context.Context, string, ...string) (string, error) {
	return func(_ context.Context, dir string, args ...string) (string, error) {
		*calls = append(*calls, gitCall{dir, args})
		return out, err
	}
}

func TestResultCommits(t *testing.T) {
	f, _, _ := newFleet(t)
	f.cfg.Code = "/code"
	ctx := context.Background()
	_ = f.Assign(ctx, "agent-03", "T1", "operator", "", t0)
	_ = f.db.SetTaskRepo("T1", "app")
	_ = f.Tell(ctx, "agent-03", "operator", "done, see branch", t0)
	_ = f.Done("agent-03", "T1", "done", "fleet/agent-03/x", "", t0)
	var calls []gitCall
	f.git = fakeGit(&calls, "abc1234 fix the thing\ndef5678 add a test\n", nil)

	r, err := f.Result("T1")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "closed" || r.Task.ID != "T1" || len(r.Reports) != 1 || r.Reports[0].Text != "done, see branch" {
		t.Fatalf("result = %+v", r)
	}
	if r.Ahead != 2 || len(r.Commits) != 2 || r.Commits[0] != "abc1234 fix the thing" || r.CommitsError != "" {
		t.Fatalf("commits = %+v", r)
	}
	if len(calls) != 1 || calls[0].dir != "/code/app" ||
		strings.Join(calls[0].args, " ") != "log --format=%h %s main..fleet/agent-03/x" {
		t.Fatalf("git calls = %+v", calls)
	}
}

func TestResultOpenTaskHasState(t *testing.T) {
	f, _, _ := newFleet(t)
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	r, err := f.Result("T1")
	if err != nil || r.State != "open" || r.Reports == nil || len(r.Commits) != 0 {
		t.Fatalf("result = %+v, %v", r, err)
	}
	if _, err := f.Result("T404"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("err = %v", err)
	}
}

func TestResultNoBranch(t *testing.T) {
	f, _, _ := newFleet(t)
	f.cfg.Code = "/code"
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	_ = f.db.SetTaskRepo("T1", "app") // a repo but no branch
	var calls []gitCall
	f.git = fakeGit(&calls, "x", nil)
	r, err := f.Result("T1")
	if err != nil || len(calls) != 0 || len(r.Commits) != 0 || r.CommitsError != "" {
		t.Fatalf("result = %+v, %v, git %v", r, err, calls)
	}
}

func TestResultGitFailureIsReportedNotFatal(t *testing.T) {
	f, _, _ := newFleet(t)
	f.cfg.Code = "/code"
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	_ = f.db.SetTaskRepo("T1", "app")
	_ = f.Done("agent-03", "T1", "done", "gone", "", t0)
	var calls []gitCall
	f.git = fakeGit(&calls, "", errors.New("unknown revision"))
	r, err := f.Result("T1")
	if err != nil || len(r.Commits) != 0 || !strings.Contains(r.CommitsError, "unknown revision") {
		t.Fatalf("result = %+v, %v", r, err)
	}
}

func TestResultRejectsABranchThatLooksLikeAnOption(t *testing.T) {
	f, _, _ := newFleet(t)
	f.cfg.Code = "/code"
	_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
	_ = f.db.SetTaskRepo("T1", "app")
	_ = f.Done("agent-03", "T1", "done", "--output=/x", "", t0)
	var calls []gitCall
	f.git = fakeGit(&calls, "x y", nil)
	if r, _ := f.Result("T1"); len(calls) != 0 || r.CommitsError == "" {
		t.Fatalf("result = %+v, git %v", r, calls)
	}
}

func TestResultRejectsRepoTraversal(t *testing.T) {
	for _, repo := range []string{"../etc", "a/../b", "/abs", ".."} {
		f, _, _ := newFleet(t)
		f.cfg.Code = "/code"
		_ = f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0)
		_ = f.db.SetTaskRepo("T1", repo)
		_ = f.Done("agent-03", "T1", "done", "b", "", t0)
		var calls []gitCall
		f.git = fakeGit(&calls, "x y", nil)
		r, err := f.Result("T1")
		if err != nil || len(calls) != 0 || len(r.Commits) != 0 || r.CommitsError == "" {
			t.Errorf("repo %q: result = %+v, %v, git %v", repo, r, err, calls)
		}
	}
}
