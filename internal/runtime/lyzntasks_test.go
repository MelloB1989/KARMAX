package runtime

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func TestPruneStaleLyznSessionsDeletesMatchingRows(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "test.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.SaveCodingSession(store.StoredCodingSession{
		ID: "row-1", ToolType: "claude_code", SessionID: "lyzn:task-9", AgentID: "a1",
	}); err != nil {
		t.Fatalf("save lyzn row: %v", err)
	}
	if err := s.SaveCodingSession(store.StoredCodingSession{
		ID: "row-2", ToolType: "claude_code", SessionID: "other:task-1", AgentID: "a1",
	}); err != nil {
		t.Fatalf("save other row: %v", err)
	}

	rt := &KarmaxRuntime{store: s, log: zap.NewNop()}
	rt.pruneStaleLyznSessions(time.Now().Add(time.Hour)) // cutoff in the future: both rows already qualify by age

	left, err := s.ListCodingSessions("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 1 || left[0].SessionID != "other:task-1" {
		t.Fatalf("got %v, want only the non-lyzn row left", left)
	}
}
