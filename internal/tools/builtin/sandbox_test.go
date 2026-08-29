package builtin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// The failure this tool exists to prevent: asked to spin up a sandbox, the
// agent had no tool for it and narrated the action instead — reporting a
// branch and a PR that did not exist. So the tool must actually launch.
func TestStartLaunchesTheSandbox(t *testing.T) {
	var got loopkit.SandboxSpec
	launched := make(chan struct{})
	tool := &SandboxTool{
		AgentID: "ocrew",
		Launch: func(_ context.Context, _ string, spec loopkit.SandboxSpec) (loopkit.SandboxResult, error) {
			got = spec
			close(launched)
			return loopkit.SandboxResult{RunID: "r1", Status: "exited"}, nil
		},
		Publish: func(bus.Event) error { return nil },
	}

	res, err := tool.Execute(context.Background(), map[string]any{
		"repo": "dev-zeromoblt/o-refine-react", "task": "implement RTE-17",
		"branch": "feature/RTE-17", "base_branch": "main", "case_id": "RTE-17",
	})
	if err != nil || res.IsError {
		t.Fatalf("start failed: %v %v", err, res.Error)
	}

	select {
	case <-launched:
	case <-time.After(2 * time.Second):
		t.Fatal("the tool returned success without ever launching a sandbox")
	}
	if got.Repo != "dev-zeromoblt/o-refine-react" || got.Task != "implement RTE-17" {
		t.Errorf("the brief did not reach the container: %+v", got)
	}
	if got.Branch != "feature/RTE-17" || got.CaseID != "RTE-17" {
		t.Errorf("branch/case lost: %+v", got)
	}
	// BASE_BRANCH is the name the entrypoint reads; a different key silently
	// starts every run from the default branch.
	if got.Env["BASE_BRANCH"] != "main" {
		t.Errorf("base branch not passed as BASE_BRANCH: %+v", got.Env)
	}
}

// The agent must not be able to report success before the run ends.
func TestStartReturnsBeforeTheRunFinishes(t *testing.T) {
	release := make(chan struct{})
	tool := &SandboxTool{
		Launch: func(context.Context, string, loopkit.SandboxSpec) (loopkit.SandboxResult, error) {
			<-release
			return loopkit.SandboxResult{RunID: "r1", Status: "exited"}, nil
		},
		Publish: func(bus.Event) error { return nil },
	}
	done := make(chan struct{})
	go func() {
		_, _ = tool.Execute(context.Background(), map[string]any{"repo": "o/r", "task": "t"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("start blocked on the build; the whole conversation would wait")
	}
	close(release)
}

// A failed build has to reach the agent as a failure, or it reports a PR that
// was never opened.
func TestAFailedRunIsAnnouncedAsFailed(t *testing.T) {
	for name, launch := range map[string]func(context.Context, string, loopkit.SandboxSpec) (loopkit.SandboxResult, error){
		"driver error": func(context.Context, string, loopkit.SandboxSpec) (loopkit.SandboxResult, error) {
			return loopkit.SandboxResult{}, errors.New("no such image")
		},
		"nonzero exit": func(context.Context, string, loopkit.SandboxSpec) (loopkit.SandboxResult, error) {
			return loopkit.SandboxResult{RunID: "r1", Status: "exited", ExitCode: 1, LogTail: "tests failed"}, nil
		},
	} {
		events := make(chan bus.Event, 1)
		tool := &SandboxTool{Launch: launch, Publish: func(e bus.Event) error { events <- e; return nil }}
		if _, err := tool.Execute(context.Background(), map[string]any{"repo": "o/r", "task": "t"}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		select {
		case e := <-events:
			if e.Payload["status"] != "failed" {
				t.Errorf("%s: announced as %v, so the agent would claim success", name, e.Payload["status"])
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s: nothing was announced; the agent waits forever", name)
		}
	}
}

func TestStartRefusesAnEmptyBrief(t *testing.T) {
	tool := &SandboxTool{Launch: func(context.Context, string, loopkit.SandboxSpec) (loopkit.SandboxResult, error) {
		t.Fatal("launched with no repo or task")
		return loopkit.SandboxResult{}, nil
	}}
	for _, in := range []map[string]any{{"repo": "o/r"}, {"task": "t"}, {}} {
		if res, _ := tool.Execute(context.Background(), in); !res.IsError {
			t.Errorf("accepted an incomplete brief: %v", in)
		}
	}
}

// No driver on the host must be an error the agent sees, not a silent success.
func TestNoDriverIsReportedNotSwallowed(t *testing.T) {
	res, err := (&SandboxTool{}).Execute(context.Background(), map[string]any{"repo": "o/r", "task": "t"})
	if err != nil || !res.IsError {
		t.Fatalf("a host with no sandbox driver reported success: %v %+v", err, res)
	}
}
