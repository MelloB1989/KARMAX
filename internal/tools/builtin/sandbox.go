package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// The sandbox was reachable only from loops and recipes. An agent asked to
// "spin up a sandbox and raise a PR" had no tool for it, and said it had done
// so anyway — reporting a branch and a PR that did not exist. A capability the
// agent is asked to use has to be a tool the agent can call.

// SandboxTool launches a container that clones a repo, runs a coding agent
// against a task, and pushes a branch.
type SandboxTool struct {
	Store   *store.Store
	AgentID string
	// Launch runs a sandbox to completion. Wired by the runtime after
	// construction: the driver belongs to the runtime, which does not exist
	// when the tool is registered.
	Launch func(ctx context.Context, agent string, spec loopkit.SandboxSpec) (loopkit.SandboxResult, error)
	// Publish delivers the outcome once the run ends. Without it the tool can
	// only run inline, which holds the turn open for the length of the build.
	Publish func(bus.Event) error
}

func (t *SandboxTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "sandbox.start",
		Description: "Spin up an isolated sandbox container that clones a repo, runs a coding agent against a task, commits to a branch and pushes it. " +
			"Use this for any request to implement, fix, or change code in a repository — it is the ONLY way to actually do that work. " +
			"Returns a run_id immediately and keeps running in the background; the outcome arrives later as a delegation.completed event. " +
			"Never tell the operator the work is done, the branch exists, or a PR is open until that event arrives.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"repo": {"type": "string", "description": "Repository as owner/name (e.g. dev-zeromoblt/o-refine-react) or a clone URL."},
				"task": {"type": "string", "description": "What the coding agent should do, in prose. Include the acceptance criteria — this is the whole brief it gets."},
				"branch": {"type": "string", "description": "Branch to create and push. Defaults to a generated karmax/sandbox-... name."},
				"base_branch": {"type": "string", "description": "Existing branch to start from and open the PR against. Defaults to main."},
				"case_id": {"type": "string", "description": "Ticket or case this run belongs to (e.g. RTE-17), so the run is traceable to the work item."},
				"timeout_minutes": {"type": "number", "description": "Give up after this long. Defaults to 45."},
				"wait": {"type": "boolean", "description": "Block until the sandbox finishes instead of returning a run_id. Holds the conversation open for the whole build — only for short runs."}
			},
			"required": ["repo", "task"]
		}`),
	}
}

func (t *SandboxTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	if t.Launch == nil {
		return tools.ErrorResult(errors.New("sandbox is not available: no driver is wired on this host")), nil
	}
	repo, _ := input["repo"].(string)
	task, _ := input["task"].(string)
	if repo == "" || task == "" {
		return tools.ErrorResult(errors.New("sandbox.start needs both repo and task")), nil
	}

	// Spec.Branch is the BASE. Both drivers map it straight to BASE_BRANCH,
	// which the entrypoint clones with --branch, and the entrypoint takes the
	// branch it CREATES from WORK_BRANCH instead. Passing the new branch as
	// Spec.Branch clones a ref that does not exist yet and fails every run.
	base := str(input["base_branch"])
	if base == "" {
		base = "main"
	}
	spec := loopkit.SandboxSpec{
		Repo:    repo,
		Task:    task,
		CaseID:  str(input["case_id"]),
		Branch:  base,
		Timeout: 45 * time.Minute,
	}
	if mins, ok := input["timeout_minutes"].(float64); ok && mins > 0 {
		spec.Timeout = time.Duration(mins) * time.Minute
	}
	if work := str(input["branch"]); work != "" {
		spec.Env = map[string]string{"WORK_BRANCH": work}
	}

	if wait, _ := input["wait"].(bool); wait || t.Publish == nil {
		res, err := t.Launch(ctx, t.AgentID, spec)
		if err != nil {
			return tools.ErrorResult(err), nil
		}
		return tools.SuccessResult(map[string]any{
			"run_id": res.RunID, "status": res.Status,
			"exit_code": res.ExitCode, "log_tail": res.LogTail,
		}), nil
	}
	return t.startBackground(spec), nil
}

// startBackground runs the sandbox past the end of the turn. A build takes
// minutes; inline, it holds the agent's session for all of them and every other
// chat waits.
func (t *SandboxTool) startBackground(spec loopkit.SandboxSpec) tools.ToolResult {
	go func() {
		// Not the caller's context: it dies with the turn, which is the point
		// of running this after the turn is over.
		ctx, cancel := context.WithTimeout(context.Background(), spec.Timeout+2*time.Minute)
		defer cancel()

		res, err := t.Launch(ctx, t.AgentID, spec)
		payload := map[string]any{
			"tool": "sandbox.start",
			"task": fmt.Sprintf("sandbox on %s: %s", spec.Repo, truncate(spec.Task, 400)),
		}
		switch {
		case err != nil:
			payload["status"], payload["error"] = "failed", err.Error()
		case res.ExitCode != 0 || res.Status != "exited":
			payload["status"] = "failed"
			payload["error"] = fmt.Sprintf("sandbox %s with exit code %d\n\n%s",
				res.Status, res.ExitCode, truncate(res.LogTail, 3000))
		default:
			payload["status"] = "completed"
			payload["output"] = truncate(res.LogTail, 4000)
		}
		payload["run_id"] = res.RunID
		if err := t.Publish(bus.NewEvent(bus.EventDelegationDone, t.AgentID, payload)); err != nil {
			fmt.Fprintf(os.Stderr, "karmax: sandbox run %s finished but could not be announced: %v\n", res.RunID, err)
		}
	}()

	return tools.SuccessResult(map[string]any{
		"status": "started",
		"note": "The sandbox is starting. You will receive a delegation.completed event with the outcome. " +
			"Tell the operator it is underway; do NOT claim the branch, commit or PR exists until that event arrives.",
	})
}

// SandboxStatusTool answers "where did that run get to" without waiting.
type SandboxStatusTool struct {
	Store *store.Store
}

func (t *SandboxStatusTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "sandbox.status",
		Description: "Check how a sandbox run is going, by run_id. Reports status, exit code and the tail of its log.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {"run_id": {"type": "string", "description": "The run_id returned by sandbox.start."}},
			"required": ["run_id"]
		}`),
	}
}

func (t *SandboxStatusTool) Execute(_ context.Context, input map[string]any) (tools.ToolResult, error) {
	id := str(input["run_id"])
	if id == "" {
		return tools.ErrorResult(errors.New("sandbox.status needs a run_id")), nil
	}
	run, found, err := t.Store.SandboxRun(id)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if !found {
		return tools.ErrorResult(errors.New("no sandbox run with id " + id)), nil
	}
	return tools.SuccessResult(map[string]any{
		"run_id": run.ID, "status": run.Status, "exit_code": run.ExitCode,
		"repo": run.Repo, "branch": run.Branch, "error": run.Error,
		"log_tail": truncate(run.LogTail, 3000), "started_at": run.StartedAt,
	}), nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
