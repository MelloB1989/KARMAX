package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/google/uuid"
)

type CodexTool struct {
	Store   *store.Store
	AgentID string
}

func (t *CodexTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "codex.call",
		Description: "Call Codex CLI to perform coding tasks. Can create new sessions or continue existing ones.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"prompt": {"type": "string", "description": "The coding task or prompt to send to Codex"},
				"session_id": {"type": "string", "description": "Optional: continue an existing session. Leave empty for new session."},
				"working_dir": {"type": "string", "description": "Working directory for the coding task"}
			},
			"required": ["prompt"]
		}`),
	}
}

func (t *CodexTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	prompt, _ := input["prompt"].(string)
	if prompt == "" {
		return tools.ErrorResult(fmt.Errorf("prompt is required")), nil
	}

	sessionID, _ := input["session_id"].(string)
	resumedFrom := ""
	if sessionID == "" {
		if reusable := findReusableCodingSession(t.Store, t.AgentID, "codex", prompt); reusable != nil {
			sessionID = reusable.SessionID
			resumedFrom = reusable.ID
			prompt = prependSessionContext(prompt, reusable)
		}
	}

	workingDir, _ := input["working_dir"].(string)
	if workingDir == "" {
		workingDir = "/home/mellob"
	}

	// Codex CLI uses --quiet for non-interactive output
	args := []string{"--quiet", prompt}

	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "codex", args...)
	cmd.Dir = workingDir
	cmd.Env = harnessEnv() // use codex's own auth, not KARMAX's gateway

	output, err := cmd.CombinedOutput()

	// Codex does not support session continuation natively;
	// generate a tracking ID for internal session bookkeeping.
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
			ToolType:    "codex",
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
