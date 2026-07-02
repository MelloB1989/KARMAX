package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/google/uuid"
)

type ClaudeCodeTool struct {
	Store   *store.Store
	AgentID string
	// Namespace is the memory namespace whose profile/entries are injected into
	// every call. Set per-agent in bindAgentTools; falls back to AgentID.
	Namespace string
}

func (t *ClaudeCodeTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "claude_code.call",
		Description: "Delegate a coding/engineering task to the Claude Code CLI — a full coding agent with file, shell, and web tools, running on the operator's Claude subscription. " +
			"CONTINUITY IS IMPORTANT: to continue earlier work on the same project/feature, pass its session_id (find it in the '## Active Coding Sessions' list in your context, matched by the task description) — this resumes the exact session with all prior context, even days later. " +
			"Omit session_id ONLY for genuinely new, unrelated work; otherwise KARMAX auto-resumes the closest matching prior session. Set ephemeral=true for one-off tasks whose session has no follow-up value (it is deleted afterwards). Prefer this over codex.call.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"prompt": {"type": "string", "description": "The coding task or follow-up instruction to send to Claude Code"},
				"session_id": {"type": "string", "description": "To CONTINUE prior work: the session_id from '## Active Coding Sessions' in your context. Omit only for brand-new, unrelated tasks."},
				"working_dir": {"type": "string", "description": "Working directory for the coding task"},
				"ephemeral": {"type": "boolean", "description": "One-off task: don't keep the session for resumption; its transcript is deleted after the run."}
			},
			"required": ["prompt"]
		}`),
	}
}

// memoryContext builds the KARMAX context block injected into EVERY Claude Code
// call: the operator profile, memory entries relevant to the prompt, and how to
// self-serve more via the karmax CLI. This is what makes the executor act with
// the operator's full context instead of cold.
func (t *ClaudeCodeTool) memoryContext(prompt string) string {
	ns := t.Namespace
	if ns == "" {
		ns = t.AgentID
	}
	if ns == "" {
		ns = discoverNamespace()
	}
	var sb strings.Builder
	sb.WriteString("# KARMAX context (auto-injected)\n\n")

	if home, err := os.UserHomeDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, ".karmax", "memory", ns, "ABOUT_ME.md")); err == nil {
			p := strings.TrimSpace(string(b))
			if len(p) > 1500 {
				p = p[:1500] + "…"
			}
			if p != "" {
				sb.WriteString("## Operator profile\n" + p + "\n\n")
			}
		}
	}

	if t.Store != nil {
		seen := map[string]bool{}
		var hits []string
		for _, kw := range pickKeywords(prompt, 4) {
			entries, _ := t.Store.SearchMemoryEntries(ns, kw, 4)
			for _, e := range entries {
				if seen[e.ID] {
					continue
				}
				seen[e.ID] = true
				hits = append(hits, "- "+truncate(e.Content, 200))
			}
		}
		if len(hits) > 8 {
			hits = hits[:8]
		}
		if len(hits) > 0 {
			sb.WriteString("## Possibly relevant memory\n" + strings.Join(hits, "\n") + "\n\n")
		}
	}

	karmaxBin := hostpaths.KarmaxBin()
	sb.WriteString("## KARMAX CLI — full harness access\n" +
		"You can reach EVERYTHING the KARMAX harness can do through its CLI at `" + karmaxBin + "` (talks to the running daemon; auth is picked up from the environment):\n" +
		"- `" + karmaxBin + " memory search \"<query>\"` — search the operator's long-term memory. Use before acting when you need context about people, projects, commitments, or preferences.\n" +
		"- `" + karmaxBin + " memory add \"<fact>\" [--category <c>] [--importance <i>]` — save a durable fact you learned while working.\n" +
		"- `" + karmaxBin + " notify \"<title>\" \"<body>\"` — notify the operator via their phone app (feed + push). Use for results, alerts, or anything they should see.\n" +
		"- `" + karmaxBin + " send \"<target>\" \"<message>\"` — send a WhatsApp message through the operator's account.\n" +
		"- `" + karmaxBin + " ask \"<prompt>\"` — ask the orchestrator agent (it has the operator's full context and judgement).\n" +
		"- `" + karmaxBin + " tool list` and `" + karmaxBin + " tool call <name> --json '<input>'` — list and invoke ANY harness tool (calendar.add, reminder.add, propose, google_workspace, whatsapp.read, scheduler.add, …).\n\n" +
		"----\n\n# TASK\n\n")
	return sb.String()
}

// discoverNamespace finds the memory namespace when none was injected: the
// single directory under ~/.karmax/memory (ambiguous → empty, inject nothing).
func discoverNamespace() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(filepath.Join(home, ".karmax", "memory"))
	if err != nil {
		return ""
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 1 {
		return dirs[0]
	}
	return ""
}

// pickKeywords extracts up to n distinctive words (>4 chars) from a prompt for
// best-effort memory prefetch.
func pickKeywords(s string, n int) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.Fields(strings.ToLower(s)) {
		w = strings.Trim(w, ".,;:!?\"'()[]{}*`")
		if len(w) <= 4 || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
		if len(out) >= n {
			break
		}
	}
	return out
}

func (t *ClaudeCodeTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	prompt, _ := input["prompt"].(string)
	if prompt == "" {
		return tools.ErrorResult(fmt.Errorf("prompt is required")), nil
	}

	ephemeral, _ := input["ephemeral"].(bool)

	sessionID, _ := input["session_id"].(string)
	resumedFrom := ""
	resuming := sessionID != ""
	// Ephemeral one-off tasks never reuse or become resumable sessions.
	if sessionID == "" && !ephemeral {
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
		workingDir = hostpaths.WorkDir()
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
	// Every call carries the operator's KARMAX context (profile + relevant
	// memory + how to query more), so the executor never starts cold.
	args = append(args, t.memoryContext(prompt)+prompt)

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

	if ephemeral {
		// One-off task: the session has no follow-up value — delete the
		// transcript and don't persist it as a resumable coding session.
		removeClaudeSession(workingDir, sessionID)
	} else if t.Store != nil {
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
		"ephemeral":    ephemeral,
	}), nil
}

// removeClaudeSession deletes a Claude Code session transcript
// (~/.claude/projects/<dir-slug>/<session-id>.jsonl).
func removeClaudeSession(workingDir, sessionID string) {
	if sessionID == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	slug := strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(workingDir)
	path := filepath.Join(home, ".claude", "projects", slug, sessionID+".jsonl")
	_ = os.Remove(path)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
