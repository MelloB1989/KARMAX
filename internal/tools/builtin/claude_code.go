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
		Name: "claude_code.call",
		Description: "Delegate a coding/engineering task to the Claude Code CLI — a full coding agent with file, shell, and web tools, running on the operator's Claude subscription. " +
			"CONTINUITY IS IMPORTANT: to continue earlier work on the same project/feature, pass its session_id (find it in the '## Active Coding Sessions' list in your context, matched by the task description) — this resumes the exact session with all prior context, even days later. " +
			"Omit session_id ONLY for genuinely new, unrelated work; otherwise KARMAX auto-resumes the closest matching prior session. Prefer this over codex.call.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"prompt": {"type": "string", "description": "The coding task or follow-up instruction to send to Claude Code"},
				"session_id": {"type": "string", "description": "To CONTINUE prior work: the session_id from '## Active Coding Sessions' in your context. Omit only for brand-new, unrelated tasks."},
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
	//
	// Session args differ by case (current Claude CLI):
	//   - new session:  --session-id <uuid>   (pre-mint an id we can resume later)
	//   - resume:       --resume <uuid>        (the id is the VALUE of --resume;
	//                   "--session-id X --resume" is rejected by the CLI)
	args := []string{"--print", "--output-format", "text", "--dangerously-skip-permissions"}
	if resuming {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
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
