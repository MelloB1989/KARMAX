package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/google/uuid"
)

type ClaudeCodeTool struct {
	Store   *store.Store
	AgentID string
}

func (t *ClaudeCodeTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "claude_code.call",
		Description: "Call Claude Code CLI to perform coding tasks. Can create new sessions or continue existing ones. Session IDs are tracked for continuity.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"prompt": {"type": "string", "description": "The coding task or prompt to send to Claude Code"},
				"session_id": {"type": "string", "description": "Optional: continue an existing session. Leave empty for new session."},
				"working_dir": {"type": "string", "description": "Working directory for the coding task"}
			},
			"required": ["prompt"]
		}`),
	}
}

func (t *ClaudeCodeTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	prompt, _ := input["prompt"].(string)
	if prompt == "" {
		return tools.ErrorResult(fmt.Errorf("prompt is required")), nil
	}

	sessionID, _ := input["session_id"].(string)
	resumedFrom := ""
	resuming := sessionID != ""
	if sessionID == "" {
		if reusable := findReusableCodingSession(t.Store, t.AgentID, "claude_code", prompt); reusable != nil {
			sessionID = reusable.SessionID
			resumedFrom = reusable.ID
			resuming = true
		}
	}

	// Pre-generate a stable session ID for new sessions so subsequent calls can
	// resume them. Passing --session-id up front (instead of letting Claude Code
	// mint its own and then discarding it) is what makes --resume work later.
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	workingDir, _ := input["working_dir"].(string)
	if workingDir == "" {
		workingDir = "/home/mellob"
	}

	// --dangerously-skip-permissions lets the headless harness actually use its
	// tools (WebSearch/WebFetch, file, bash) without an interactive permission
	// prompt — without it, --print mode silently blocks web search and the agent
	// concludes "web is unavailable".
	args := []string{"--print", "--output-format", "text", "--dangerously-skip-permissions", "--session-id", sessionID}
	if resuming {
		args = append(args, "--resume")
	}
	args = append(args, prompt)

	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "claude", args...)
	cmd.Dir = workingDir
	cmd.Env = harnessEnv() // use claude's own auth, not KARMAX's gateway

	output, err := cmd.CombinedOutput()

	status := "completed"
	if err != nil {
		status = "failed"
	}

	if t.Store != nil {
		_ = t.Store.SaveCodingSession(store.StoredCodingSession{
			ID:          uuid.New().String(),
			ToolType:    "claude_code",
			SessionID:   sessionID,
			Description: truncate(prompt, 200),
			Status:      status,
			AgentID:     t.AgentID,
			Output:      truncate(string(output), 5000),
		})
	}

	return tools.SuccessResult(map[string]any{
		"session_id":   sessionID,
		"resumed_from": resumedFrom,
		"output":       string(output),
		"status":       status,
	}), nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
