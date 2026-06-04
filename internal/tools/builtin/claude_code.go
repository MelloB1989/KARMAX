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
	if sessionID == "" {
		if reusable := findReusableCodingSession(t.Store, t.AgentID, "claude_code", prompt); reusable != nil {
			sessionID = reusable.SessionID
			resumedFrom = reusable.ID
		}
	}

	workingDir, _ := input["working_dir"].(string)
	if workingDir == "" {
		workingDir = "/home/mellob"
	}

	args := []string{"--print", "--output-format", "text"}
	if sessionID != "" {
		args = append(args, "--session-id", sessionID, "--resume")
	}
	args = append(args, prompt)

	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "claude", args...)
	cmd.Dir = workingDir

	output, err := cmd.CombinedOutput()

	if sessionID == "" {
		sessionID = uuid.New().String()
	}

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
