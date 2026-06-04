package builtin

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func TestFindReusableCodingSession(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "karmax.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	if err := s.SaveCodingSession(store.StoredCodingSession{
		ID:          "old-session",
		ToolType:    "claude_code",
		SessionID:   "claude-123",
		Description: "fix checkout bug in billing service",
		Status:      "completed",
		AgentID:     "agent-main",
		Output:      "patched checkout validation",
	}); err != nil {
		t.Fatalf("SaveCodingSession: %v", err)
	}
	if err := s.SaveCodingSession(store.StoredCodingSession{
		ID:          "failed-session",
		ToolType:    "claude_code",
		SessionID:   "claude-failed",
		Description: "fix checkout bug in billing service",
		Status:      "failed",
		AgentID:     "agent-main",
	}); err != nil {
		t.Fatalf("SaveCodingSession failed row: %v", err)
	}

	reusable := findReusableCodingSession(s, "agent-main", "claude_code", "continue fixing checkout billing bug")
	if reusable == nil {
		t.Fatal("expected reusable coding session")
	}
	if reusable.SessionID != "claude-123" {
		t.Fatalf("expected claude-123, got %s", reusable.SessionID)
	}
}

func TestPrependSessionContext(t *testing.T) {
	prompt := prependSessionContext("finish tests", &store.StoredCodingSession{
		SessionID:   "codex-1",
		Description: "implement message routing",
		Output:      "added manager send path",
	})

	if !strings.Contains(prompt, "codex-1") || !strings.Contains(prompt, "finish tests") {
		t.Fatalf("session context did not include prior/current details: %s", prompt)
	}
}
