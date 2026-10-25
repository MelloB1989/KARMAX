package daemon

import (
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

func TestActivityActsOutWhatTheWorkerIsDoing(t *testing.T) {
	now := t0
	fresh := now.Add(-5 * time.Second)
	working := Row{Agent: "agent-03", State: "working"}
	cases := []struct {
		name string
		row  Row
		last ToolEvent
		want fleetapi.ActivityKind
	}{
		{"a Read is reading", working, ToolEvent{Tool: "Read", At: fresh}, fleetapi.ActReading},
		{"a Grep is reading", working, ToolEvent{Tool: "Grep", At: fresh}, fleetapi.ActReading},
		{"a Glob is reading", working, ToolEvent{Tool: "Glob", At: fresh}, fleetapi.ActReading},
		{"a WebFetch is reading", working, ToolEvent{Tool: "WebFetch", At: fresh}, fleetapi.ActReading},
		{"a WebSearch is reading", working, ToolEvent{Tool: "WebSearch", At: fresh}, fleetapi.ActReading},
		{"an Edit is editing", working, ToolEvent{Tool: "Edit", At: fresh}, fleetapi.ActEditing},
		{"a Write is editing", working, ToolEvent{Tool: "Write", At: fresh}, fleetapi.ActEditing},
		{"a NotebookEdit is editing", working, ToolEvent{Tool: "NotebookEdit", At: fresh}, fleetapi.ActEditing},
		{"a Bash is running", working, ToolEvent{Tool: "Bash", Command: "ls -la", At: fresh}, fleetapi.ActRunning},
		{"a Bash with no command is running", working, ToolEvent{Tool: "Bash", At: fresh}, fleetapi.ActRunning},
		{"any other tool is running", working, ToolEvent{Tool: "Task", At: fresh}, fleetapi.ActRunning},
		{"git push is pushing", working, ToolEvent{Tool: "Bash", Command: "git push -u origin fleet/agent-03/x", At: fresh}, fleetapi.ActPushing},
		{"a git push inside && is pushing", working, ToolEvent{Tool: "Bash", Command: "cd /home/op/code/app && git add -A && git commit -m x && git push", At: fresh}, fleetapi.ActPushing},
		{"git -C dir push is pushing", working, ToolEvent{Tool: "Bash", Command: "git -C /home/op/code/app push origin HEAD", At: fresh}, fleetapi.ActPushing},
		{"go test is testing", working, ToolEvent{Tool: "Bash", Command: "go test -race ./...", At: fresh}, fleetapi.ActTesting},
		{"kbuild go test is testing", working, ToolEvent{Tool: "Bash", Command: "cd fleet && kbuild go test -race ./internal/...", At: fresh}, fleetapi.ActTesting},
		{"a quoted npm test is testing", working, ToolEvent{Tool: "Bash", Command: `kbuild "npm ci && npm test"`, At: fresh}, fleetapi.ActTesting},
		{"pnpm test is testing", working, ToolEvent{Tool: "Bash", Command: "pnpm test", At: fresh}, fleetapi.ActTesting},
		{"pytest is testing", working, ToolEvent{Tool: "Bash", Command: "python -m pytest -q tests/", At: fresh}, fleetapi.ActTesting},
		{"cargo test is testing", working, ToolEvent{Tool: "Bash", Command: "cargo test --all", At: fresh}, fleetapi.ActTesting},
		{"make test is testing", working, ToolEvent{Tool: "Bash", Command: "make test", At: fresh}, fleetapi.ActTesting},
		{"vitest is testing", working, ToolEvent{Tool: "Bash", Command: "npx vitest run", At: fresh}, fleetapi.ActTesting},
		{"a word that only contains a test command is running", working, ToolEvent{Tool: "Bash", Command: "echo nogo testing; cat mypytest.txt", At: fresh}, fleetapi.ActRunning},
		{"a stale tool is thinking", working, ToolEvent{Tool: "Edit", At: now.Add(-21 * time.Second)}, fleetapi.ActThinking},
		{"no tool at all is thinking", working, ToolEvent{}, fleetapi.ActThinking},
		{"waiting is waiting", Row{State: "waiting"}, ToolEvent{Tool: "Read", At: fresh}, fleetapi.ActWaiting},
		{"standby is idle", Row{State: "standby"}, ToolEvent{Tool: "Read", At: fresh}, fleetapi.ActIdle},
		{"cooling is cooling", Row{State: "cooling"}, ToolEvent{}, fleetapi.ActCooling},
		{"down is down", Row{State: "down"}, ToolEvent{}, fleetapi.ActDown},
		{"disabled is down", Row{State: "disabled"}, ToolEvent{}, fleetapi.ActDown},
		{"code_missing is down", Row{State: "code_missing"}, ToolEvent{}, fleetapi.ActDown},
		{"unreachable is down", Row{State: "unreachable"}, ToolEvent{}, fleetapi.ActDown},
		{"working on an unreachable host is down", Row{State: "working", Unreachable: true}, ToolEvent{Tool: "Read", At: fresh}, fleetapi.ActDown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Activity(c.row, c.last, now); got.Kind != c.want {
				t.Fatalf("Activity = %+v, want %s", got, c.want)
			}
		})
	}
}

func TestActivityNamesTheToolAndWhenItBegan(t *testing.T) {
	at := t0.Add(-3 * time.Second)
	got := Activity(Row{State: "working"}, ToolEvent{Tool: "Edit", At: at}, t0)
	if got.Tool != "Edit" || !got.Since.Equal(at) {
		t.Fatalf("Activity = %+v", got)
	}
	// Thinking since the last tool ended: the tool is still named.
	got = Activity(Row{State: "working"}, ToolEvent{Tool: "Edit", At: t0.Add(-time.Minute)}, t0)
	if got.Kind != fleetapi.ActThinking || got.Tool != "Edit" || !got.Since.Equal(t0.Add(-time.Minute)) {
		t.Fatalf("thinking = %+v", got)
	}
	// Idle since its last busy moment.
	got = Activity(Row{State: "standby", Idle: "5m", IdleS: 300}, ToolEvent{}, t0)
	if got.Kind != fleetapi.ActIdle || !got.Since.Equal(t0.Add(-5*time.Minute)) {
		t.Fatalf("idle = %+v", got)
	}
}
