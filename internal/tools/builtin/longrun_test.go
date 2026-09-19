package builtin

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"go.uber.org/zap"
)

// A turn that kicks off a long run must get back a task id pointing at a row
// already marked running — the detached process has nothing to check in
// against otherwise.
func TestTaskStartRegistersARunningTask(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "karmax.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	tool := &TaskStartTool{Store: s, AgentID: "agent-main"}
	res, err := tool.Execute(context.Background(), map[string]any{
		"goal":  "DM everyone who commented on the reel",
		"total": float64(700),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	out, ok := res.Output.(map[string]any)
	if !ok {
		t.Fatalf("output is %T, want map[string]any", res.Output)
	}
	taskID, _ := out["task_id"].(string)
	if taskID == "" {
		t.Fatal("no task_id returned")
	}

	got, ok, err := s.GetTask(taskID)
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	if got.Status != store.TaskRunning {
		t.Errorf("status = %q, want %q", got.Status, store.TaskRunning)
	}
	if got.AgentID != "agent-main" {
		t.Errorf("agent_id = %q, want agent-main", got.AgentID)
	}
	p, err := store.DecodeProgress(got.Progress)
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 700 {
		t.Errorf("progress.total = %d, want 700", p.Total)
	}
}

// A missing goal must fail loudly rather than register a task nobody can
// identify later.
func TestTaskStartRequiresAGoal(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "karmax.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	tool := &TaskStartTool{Store: s}
	res, err := tool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("expected an error for a missing goal")
	}
}

func newLongRunTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "karmax.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func outputMap(t *testing.T, res tools.ToolResult) map[string]any {
	t.Helper()
	out, ok := res.Output.(map[string]any)
	if !ok {
		t.Fatalf("output is %T, want map[string]any", res.Output)
	}
	return out
}

// task.progress is the whole point of Task 10: a paced send writes its
// ledger and, in the very same call, learns whether to keep going. One tool
// call per recipient, not a write followed by a separate read.
func TestTaskProgressWritesAndReturnsCurrentStatus(t *testing.T) {
	s := newLongRunTestStore(t)
	task, err := s.CreateTask(store.Task{Goal: "paced send", Status: store.TaskRunning,
		Progress: store.EncodeProgress(store.TaskProgress{Total: 700})})
	if err != nil {
		t.Fatal(err)
	}

	tool := &TaskProgressTool{Store: s}
	res, err := tool.Execute(context.Background(), map[string]any{
		"task_id": task.ID, "sent": float64(13), "attempted": float64(13),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	out := outputMap(t, res)
	if out["status"] != store.TaskRunning {
		t.Errorf("status = %v, want %q", out["status"], store.TaskRunning)
	}
	if out["sent"] != 13 || out["attempted"] != 13 {
		t.Errorf("sent/attempted = %v/%v, want 13/13", out["sent"], out["attempted"])
	}
	if out["total"] != 700 {
		t.Errorf("total = %v, want 700 carried over from task.start", out["total"])
	}

	got, ok, err := s.GetTask(task.ID)
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	p, err := store.DecodeProgress(got.Progress)
	if err != nil {
		t.Fatal(err)
	}
	if p.Sent != 13 || p.Attempted != 13 {
		t.Errorf("stored progress = %+v, want sent=13 attempted=13", p)
	}
}

// An unknown task_id — a typo, a task already pruned — must come back as a
// tool error a caller can act on, never a panic that takes the detached
// script down with it.
func TestTaskProgressOnAnUnknownIDIsAnErrorNotAPanic(t *testing.T) {
	s := newLongRunTestStore(t)
	tool := &TaskProgressTool{Store: s}

	res, err := tool.Execute(context.Background(), map[string]any{
		"task_id": "does-not-exist", "sent": float64(1), "attempted": float64(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("expected an error for an unknown task_id")
	}
}

// task.status with no status field is a pure read — it must not disturb the
// row it reports on.
func TestTaskStatusReadsWithoutChangingAnything(t *testing.T) {
	s := newLongRunTestStore(t)
	task, err := s.CreateTask(store.Task{Goal: "paced send", Status: store.TaskRunning})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskProgress(task.ID, store.TaskProgress{Sent: 5, Attempted: 6, Total: 700}); err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.GetTask(task.ID)

	tool := &TaskStatusTool{Store: s}
	res, err := tool.Execute(context.Background(), map[string]any{"task_id": task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	out := outputMap(t, res)
	if out["status"] != store.TaskRunning {
		t.Errorf("status = %v, want %q", out["status"], store.TaskRunning)
	}
	if out["sent"] != 5 || out["attempted"] != 6 || out["total"] != 700 {
		t.Errorf("progress in read = %v, want sent=5 attempted=6 total=700", out)
	}

	after, _, _ := s.GetTask(task.ID)
	if after.Status != before.Status || after.Progress != before.Progress || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("a read-only call changed the row: before=%+v after=%+v", before, after)
	}
}

// This is the pause button. Spec §3b: "pausing is one UPDATE" — this is where
// a conversation reaches it.
func TestTaskStatusSetsPausedAndCancelled(t *testing.T) {
	s := newLongRunTestStore(t)
	task, err := s.CreateTask(store.Task{Goal: "paced send", Status: store.TaskRunning})
	if err != nil {
		t.Fatal(err)
	}
	tool := &TaskStatusTool{Store: s}

	res, err := tool.Execute(context.Background(), map[string]any{"task_id": task.ID, "status": "paused"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error pausing: %s", res.Error)
	}
	if got, _, _ := s.GetTask(task.ID); got.Status != store.TaskPaused {
		t.Errorf("status = %q, want %q", got.Status, store.TaskPaused)
	}

	res, err = tool.Execute(context.Background(), map[string]any{"task_id": task.ID, "status": "cancelled"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error cancelling: %s", res.Error)
	}
	if got, _, _ := s.GetTask(task.ID); got.Status != store.TaskCancelled {
		t.Errorf("status = %q, want %q", got.Status, store.TaskCancelled)
	}
}

// Anything outside running/paused/cancelled must be rejected loudly and name
// the valid set, not get written into the column as garbage a reader then
// has to guess the meaning of.
func TestTaskStatusRejectsAnUnknownStatus(t *testing.T) {
	s := newLongRunTestStore(t)
	task, err := s.CreateTask(store.Task{Goal: "paced send", Status: store.TaskRunning})
	if err != nil {
		t.Fatal(err)
	}
	tool := &TaskStatusTool{Store: s}

	res, err := tool.Execute(context.Background(), map[string]any{"task_id": task.ID, "status": "banana"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected an error for an unknown status")
	}
	if !strings.Contains(res.Error, "running") || !strings.Contains(res.Error, "paused") || !strings.Contains(res.Error, "cancelled") {
		t.Errorf("error = %q, want it to name the valid statuses", res.Error)
	}
	if got, _, _ := s.GetTask(task.ID); got.Status != store.TaskRunning {
		t.Errorf("status = %q, an unknown status must not be written", got.Status)
	}
}

// Cancelling must never lose the record of who was already messaged. A
// script that has not yet noticed the cancellation writes one more progress
// ping — that write must still land, and the reply must tell it to stop.
func TestProgressOnACancelledTaskStillRecordsTheLedger(t *testing.T) {
	s := newLongRunTestStore(t)
	task, err := s.CreateTask(store.Task{Goal: "paced send", Status: store.TaskRunning,
		Progress: store.EncodeProgress(store.TaskProgress{Total: 700})})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskProgress(task.ID, store.TaskProgress{Sent: 41, Attempted: 42, Total: 700}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(task.ID, store.TaskCancelled); err != nil {
		t.Fatal(err)
	}

	tool := &TaskProgressTool{Store: s}
	res, err := tool.Execute(context.Background(), map[string]any{
		"task_id": task.ID, "sent": float64(42), "attempted": float64(43),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	out := outputMap(t, res)
	if out["status"] != store.TaskCancelled {
		t.Errorf("status = %v, want %q so the script stops", out["status"], store.TaskCancelled)
	}

	got, ok, err := s.GetTask(task.ID)
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	p, err := store.DecodeProgress(got.Progress)
	if err != nil {
		t.Fatal(err)
	}
	if p.Sent != 42 || p.Attempted != 43 {
		t.Errorf("ledger after cancel = %+v, want sent=42 attempted=43 recorded, not lost", p)
	}
}

// Listing is what lets a conversation find the task it wants to pause,
// without the operator having to already know the id.
func TestTaskStatusListsOpenLongTasksWhenNoIDIsGiven(t *testing.T) {
	s := newLongRunTestStore(t)
	running, err := s.CreateTask(store.Task{Goal: "running one", Status: store.TaskRunning})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := s.CreateTask(store.Task{Goal: "paused one", Status: store.TaskRunning})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(paused.ID, store.TaskPaused); err != nil {
		t.Fatal(err)
	}
	done, err := s.CreateTask(store.Task{Goal: "done one", Status: store.TaskRunning})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(done.ID, store.TaskCancelled); err != nil {
		t.Fatal(err)
	}

	tool := &TaskStatusTool{Store: s}
	res, err := tool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	out := outputMap(t, res)
	rows, ok := out["tasks"].([]map[string]any)
	if !ok {
		t.Fatalf("tasks is %T, want []map[string]any", out["tasks"])
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r["task_id"].(string)] = true
	}
	if !seen[running.ID] || !seen[paused.ID] {
		t.Errorf("listing = %v, want both the running and paused task", rows)
	}
	if seen[done.ID] {
		t.Errorf("listing included the cancelled task, want only running/paused")
	}
}
