package builtin

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
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
