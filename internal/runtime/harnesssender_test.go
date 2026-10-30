package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// actThenHangClaude runs a command and then never finishes the turn: the
// shape of a turn cut off after it had already acted.
func actThenHangClaude(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "acts.sh")
	body := "#!/bin/bash\n" +
		"while IFS= read -r line; do\n" +
		"  echo '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Bash\",\"input\":{\"command\":\"karmax tool call comms.send\"}}]}}'\n" +
		"  sleep 30\n" +
		"done\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The brain decides whether a failed turn may be run again on its fallback
// from the tool calls the turn made. The adapter used to drop them whenever
// the turn failed, so a turn that had already sent a message looked exactly
// like one that never started.
func TestAFailedTurnStillReportsWhatItDid(t *testing.T) {
	db, err := store.New(filepath.Join(t.TempDir(), "t.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sup := harness.New(harness.Config{
		Binary: actThenHangClaude(t), WorkdirRoot: t.TempDir(), MaxLive: 4,
		Policies: map[string]harness.Policy{"orchestrator": {TurnTimeout: 500 * time.Millisecond}},
		Env:      os.Environ(),
	}, harnessStore{db}, harness.NewBreaker(0.95, nil), harnessLog{zap.NewNop()}, nil)
	t.Cleanup(sup.Shutdown)

	send := harnessSenderFor{sup: sup, prompts: map[string]string{}}
	turn, err := send.Send(context.Background(), "agent:nexus", "orchestrator", "go ahead")
	if err == nil {
		t.Fatal("a turn that timed out came back as a success")
	}
	if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].Name != "Bash" {
		t.Errorf("tool calls = %+v, want the one Bash call the turn made before it failed", turn.ToolCalls)
	}

	// And the brain is told how long such a turn may take, so it does not
	// cut one off sooner than the kind's own configuration does.
	if got, want := send.TurnBudget("orchestrator"), sup.TurnBudget("orchestrator"); got != want {
		t.Errorf("TurnBudget = %s, want the supervisor's %s", got, want)
	}
}
