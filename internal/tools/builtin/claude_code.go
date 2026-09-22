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

	"github.com/MelloB1989/karmax/internal/browser"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/chatlog"
	instagramconn "github.com/MelloB1989/karmax/internal/connectors/instagram"
	"github.com/MelloB1989/karmax/internal/fsscope"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/memory"
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
	// MemoryMgr is what the injected memory is read from. Optional: a nil
	// manager just means the harness runs without the "possibly relevant
	// memory" block rather than with a stale one.
	MemoryMgr *memory.Manager
	// Publish delivers the result of a background delegation as an event. Nil
	// means background mode is unavailable and every call runs inline.
	Publish func(bus.Event) error
	// Browser is the operator's browser session. When it is running, the
	// harness gets it — see browserArgs.
	Browser *browser.Session
	// DataDir is where the access policy and KARMAX's own state live. Empty
	// falls back to ~/.karmax, the same as everywhere else.
	DataDir string
	// Timeout bounds a single CLI turn. Zero means the default, 10 minutes.
	// Tests shrink this to exercise the timeout path without waiting ten
	// minutes for real; it is one of three ways a run ends early, along
	// with an explicit stop and parent-context cancellation, and all three
	// go through the same process-group kill in runCLIOnce.
	Timeout time.Duration
	// EngineAPIURL and EngineBrowserToken, when both set, are handed to the
	// spawned harness as KARMAX_API_URL/KARMAX_API_TOKEN (see runCLIOnce), so
	// `karmax browser ...` run from inside it can reach this engine's own
	// API. EngineBrowserToken must always be a token scoped server-side to
	// the browser tool only (internal/api's browserScopedTools) — NEVER the
	// operator's full API token, which authorizes every connector
	// (WhatsApp, email, ...) and would hand a model-driven task the ability
	// to send messages through the user's own accounts. Leaving either empty
	// disables the injection entirely (today's behaviour: no such env vars).
	EngineAPIURL       string
	EngineBrowserToken string
}

// browserArgs attaches the operator's browser to one invocation.
//
// Only when it is actually open, and only for that run: the browser being open
// IS the grant. Somebody who has closed it has said no, and nothing has to be
// revoked anywhere for that to take effect.
//
// The order matters. --mcp-config is variadic, so it must come after the
// prompt; put it before and the CLI reads the prompt as another config file and
// fails with "MCP config file not found: <your prompt>".
func (t *ClaudeCodeTool) browserArgs(ctx context.Context) []string {
	if t.Browser == nil {
		return nil
	}
	cfg, err := t.Browser.MCPConfigJSON(ctx)
	if err != nil {
		return nil
	}
	return []string{"--mcp-config", cfg}
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
				"ephemeral": {"type": "boolean", "description": "One-off task: don't keep the session for resumption; its transcript is deleted after the run."},
				"background": {"type": "boolean", "description": "Run without waiting. Returns a job_id immediately and the result arrives later as a delegation.completed event. Use for anything long (research, multi-file changes) so you stay responsive to the operator meanwhile — you will be told the outcome, so never claim it is done before then."}
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

	// Read through the manager so the harness is given memory as it is now.
	// Querying the table directly went stale the moment GitLoom became the
	// store — and a harness handed a frozen snapshot acts on it confidently.
	if t.MemoryMgr != nil {
		seen := map[string]bool{}
		var hits []string
		for _, kw := range pickKeywords(prompt, 4) {
			results, _ := t.MemoryMgr.Search(kw, 4)
			for _, r := range results {
				if seen[r.Entry.ID] {
					continue
				}
				seen[r.Entry.ID] = true
				hits = append(hits, "- "+truncate(r.Entry.Content, 200))
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
		"- `" + karmaxBin + " tool list` and `" + karmaxBin + " tool call <name> --json '<input>'` — list and invoke ANY harness tool (calendar.add, reminder.add, propose, google, whatsapp.read, scheduler.add, …).\n" +
		"- `" + karmaxBin + " tool call dashboard --json '{\"action\":\"components\"}'`, then `save` — build a dashboard the operator sees in the desktop app.\n")
	if instagramconn.Enabled() {
		// Named here because the alternative a harness reaches for — the page
		// itself, or its own instagrapi script — has none of the pacing, the
		// cap or the ledger, and that is what got an account restricted.
		sb.WriteString("- Instagram, as the account signed into the shared browser: `" + karmaxBin +
			" tool call instagram.commenters --json '{\"url\":\"<post url>\"}'` for who commented, and " +
			"`instagram.call` with `media_comments` for the comments themselves.")
		if instagramconn.SendingEnabled() {
			sb.WriteString(" `instagram.reply_comment` and `instagram.send_dm` send ONE reply or message per call, " +
				"paced, capped and ledgered so nobody is contacted twice — use them, never the page or your own " +
				"Instagram client, and stop when one says the campaign is stopped or capped.")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n----\n\n# TASK\n\n")
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

	// A delegation can take ten minutes, and it runs inside the model turn — so
	// inline, it holds the agent's session for ten minutes and every other chat
	// waits. Backgrounding it is what keeps the agent answering meanwhile.
	if bg, _ := input["background"].(bool); bg && t.Publish != nil {
		return t.startBackground(input, prompt), nil
	}
	return t.run(ctx, input, prompt)
}

// startBackground hands the work to a goroutine and returns a handle.
func (t *ClaudeCodeTool) startBackground(input map[string]any, prompt string) tools.ToolResult {
	jobID := uuid.New().String()
	go func() {
		// Not the caller's context: it dies with the turn, which is the whole
		// point of running this after the turn is over.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()

		res, err := t.run(ctx, input, prompt)
		payload := map[string]any{
			"job_id": jobID,
			"tool":   "claude_code.call",
			"task":   truncate(prompt, 500),
		}
		switch {
		case err != nil:
			payload["status"], payload["error"] = "failed", err.Error()
		case res.IsError:
			payload["status"], payload["error"] = "failed", res.Error
		default:
			payload["status"] = "completed"
			if out, ok := res.Output.(map[string]any); ok {
				payload["output"] = out["output"]
				payload["session_id"] = out["session_id"]
			}
		}
		if err := t.Publish(bus.NewEvent(bus.EventDelegationDone, t.AgentID, payload)); err != nil {
			// The work happened; only the notification failed, and the log is
			// the only place left to say so.
			fmt.Fprintf(os.Stderr, "karmax: background delegation %s finished but could not be announced: %v\n", jobID, err)
		}
	}()

	return tools.SuccessResult(map[string]any{
		"status": "started",
		"job_id": jobID,
		"note": "Running in the background. You will receive a delegation.completed event with the result. " +
			"Do NOT claim the task is done until then — tell the operator it is underway.",
	})
}

func (t *ClaudeCodeTool) run(ctx context.Context, input map[string]any, prompt string) (tools.ToolResult, error) {
	ephemeral, _ := input["ephemeral"].(bool)

	// Checked before anything else — including creating the working
	// directory — because the race this closes is a stop landing between
	// the lyzn-tasks recipe's claim and its harness step: the run must
	// never start, not merely be interrupted once it has.
	registryKey, _ := input["session_id"].(string)
	if until, blocked := stoppedUntil(registryKey, time.Now()); blocked {
		return tools.ToolResult{IsError: true, Error: fmt.Sprintf(
			"claude_code: session %q was stopped and is blocked from new runs until %s",
			registryKey, until.Format(time.RFC3339)),
		}, nil
	}

	workingDir, _ := input["working_dir"].(string)
	workingDir = hostpaths.Resolve(workingDir)
	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		return tools.ErrorResult(fmt.Errorf("could not create working directory %s: %w", workingDir, err)), nil
	}

	rawSessionID := registryKey
	resumedFrom := ""

	// A session_id that is not a valid UUID cannot be a Claude Code session:
	// the CLI itself insists a session id "must be a valid UUID" and rejects
	// anything else, whether passed to --session-id or --resume. The LYZN
	// tasks recipe (and anything else wanting a resumable session keyed by
	// something durable of its OWN — a task id — rather than a uuid it would
	// have to invent and remember) passes a stable SESSION KEY instead. This
	// is what resolves a key to the real uuid, through the store's own
	// key -> uuid mapping (internal/store/coding_store.go).
	sessionKey := ""
	sessionID := rawSessionID
	resuming := sessionID != ""
	if sessionID != "" && !looksLikeUUID(sessionID) {
		sessionKey = sessionID
		sessionID = ""
		if t.Store != nil {
			if mapped, err := t.Store.GetSessionKey(sessionKey); err == nil && looksLikeUUID(mapped) {
				sessionID = mapped
			}
		}
		resuming = sessionID != ""
	}

	// Ephemeral one-off tasks never reuse or become resumable sessions.
	if sessionID == "" && sessionKey == "" && !ephemeral {
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

	output, cmdErr := t.runCLIOnce(ctx, workingDir, prompt, sessionID, resuming, registryKey)

	// A stale key -> uuid mapping (the transcript is gone: a Cleanup already
	// ran, the machine was migrated, the db was restored) must not fail the
	// task standing on it. Drop the mapping, mint a fresh session, and try
	// once more — recovering with lost context is the right outcome for a
	// key the mapping no longer backs, not a failure the operator sees as a
	// receipt.
	//
	// Guarded against a stop: a cancelled run's output does not, in
	// practice, contain "No conversation found", but this closes that
	// door explicitly rather than by coincidence — a stop must never be
	// followed by a retry that resurrects the run it just killed.
	contextLost := false
	if _, blocked := stoppedUntil(registryKey, time.Now()); !blocked &&
		sessionKey != "" && resuming && cmdErr != nil && looksLikeMissingSession(output) {
		contextLost = true
		if t.Store != nil {
			_ = t.Store.DeleteSessionKey(sessionKey)
		}
		sessionID = uuid.New().String()
		resuming = false
		output, cmdErr = t.runCLIOnce(ctx, workingDir, prompt, sessionID, resuming, registryKey)
	}

	status := "completed"
	if cmdErr != nil {
		status = "failed"
	}

	// Persist the key -> uuid mapping ONLY once the CLI has actually created
	// the session it just ran — confirmed by the transcript it writes to
	// disk on success, not by a clean exit code alone and never before the
	// run. Persisting before the run, or trusting exit 0 without checking,
	// leaves a mapping pointing at a session that was never created whenever
	// the first turn fails — which reproduces this exact bug on the very
	// next turn. Skipped for an ephemeral call: it deletes its own transcript
	// below, and a mapping would then point at a session already gone —
	// ephemeral sessions have no follow-up value, a mapping included.
	if sessionKey != "" && status == "completed" && t.Store != nil && !ephemeral {
		if sessionCreated(sessionID) {
			if err := t.Store.SaveSessionKey(sessionKey, sessionID, "claude_code"); err != nil {
				fmt.Fprintf(os.Stderr, "karmax: could not persist session key %q -> %s: %v\n", sessionKey, sessionID, err)
			}
		}
	}
	if contextLost {
		fmt.Fprintf(os.Stderr, "karmax: session key %q's mapped session is gone (stale --resume); "+
			"recovered with a fresh session %s instead of failing the task — prior context for this key is lost\n",
			sessionKey, sessionID)
	}

	// coding_sessions rows (the six-hour sweep, the "Active Coding Sessions"
	// list) key on whatever identity the CALLER gave this session: the
	// stable key when there is one, so the sweep's own "lyzn:" prefix match
	// keeps working unchanged; the CLI session id otherwise, exactly as
	// before.
	storedSessionID := sessionID
	if sessionKey != "" {
		storedSessionID = sessionKey
	}

	if ephemeral {
		// One-off task: the session has no follow-up value — delete the
		// transcript and don't persist it as a resumable coding session.
		chatlog.RemoveSession(workingDir, sessionID)
	} else if t.Store != nil {
		_ = t.Store.SaveCodingSession(store.StoredCodingSession{
			ID:          uuid.New().String(),
			ToolType:    "claude_code",
			SessionID:   storedSessionID,
			Description: truncate(prompt, 200),
			Status:      status,
			AgentID:     t.AgentID,
			Output:      truncate(string(output), 5000),
		})
	}

	result := map[string]any{
		"session_id":   storedSessionID,
		"resumed_from": resumedFrom,
		"output":       string(output),
		"status":       status,
		"ephemeral":    ephemeral,
	}
	if contextLost {
		result["context_lost"] = true
	}

	if status == "failed" {
		// A nonzero exit is a real failure, not something to launder into a
		// SuccessResult: HarnessWith — and every other caller — reads
		// IsError to decide whether the run actually worked, and the LYZN
		// tasks recipe's harness: step aborts (rather than POSTing this text
		// to /result as if it were the operator's answer) exactly when it
		// does.
		return tools.ToolResult{IsError: true, Error: truncate(string(output), 2000), Output: result}, nil
	}
	return tools.SuccessResult(result), nil
}

// runCLIOnce runs exactly one Claude Code CLI turn and returns its combined
// output and whether the process failed.
//
// registryKey is the caller's own identifier for this run — exactly as
// given to Execute, e.g. "lyzn:<task id>" — used only to register the run
// so harness.stop has something to find and cancel. It is deliberately not
// sessionID: sessionID may be a freshly minted uuid the caller never saw.
func (t *ClaudeCodeTool) runCLIOnce(ctx context.Context, workingDir, prompt, sessionID string, resuming bool, registryKey string) ([]byte, error) {
	// How the harness is allowed to use its tools, which depends on whether the
	// operator has said anything about what it may touch.
	//
	// With no policy: --dangerously-skip-permissions, which is what this always
	// did. Without it, --print mode silently blocks web search and the agent
	// concludes "web is unavailable".
	//
	// With a policy: --permission-mode dontAsk plus a settings blob. The two
	// cannot be combined — the dangerous flag ignores deny rules entirely, so a
	// policy passed alongside it would be decoration. dontAsk asks nobody and
	// still enforces both lists, which is the only shape that works for a
	// process with no one sitting at it.
	//
	// Session args differ by case (current Claude CLI):
	//   - new session:  --session-id <uuid>   (pre-mint an id we can resume later)
	//   - resume:       --resume <uuid>        (the id is the VALUE of --resume;
	//                   "--session-id X --resume" is rejected by the CLI)
	policy := fsscope.Load(t.DataDir)
	settings := policy.SettingsJSON(t.DataDir)
	args := []string{"--print", "--output-format", "text"}
	if settings == "" {
		args = append(args, "--dangerously-skip-permissions")
	} else {
		args = append(args, "--permission-mode", "dontAsk")
	}
	if resuming {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	// Every call carries the operator's KARMAX context (profile + relevant
	// memory + how to query more), so the executor never starts cold.
	args = append(args, t.memoryContext(prompt)+prompt)
	args = append(args, t.browserArgs(ctx)...)
	// After the prompt, like --mcp-config and for the same reason: both are
	// variadic, and a variadic flag placed before the positional prompt eats
	// it.
	if settings != "" {
		args = append(args, "--settings", settings)
		for _, dir := range policy.Dirs() {
			// One flag per directory rather than one flag with many values, so
			// nothing after it can be swallowed as another value.
			args = append(args, "--add-dir", dir)
		}
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	// timeoutCtx is done for any of three reasons — the timeout above, an
	// explicit stop calling the cancel func this run registers below, or
	// ctx itself ending (e.g. engine shutdown) — and cmd.Cancel treats all
	// three identically: SIGTERM the process group, SIGKILL it 5 seconds
	// later if anything is still alive.
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "claude", args...)
	cmd.Dir = workingDir
	cmd.Env = t.harnessCmdEnv() // use claude's own auth, not KARMAX's gateway; karmax itself resolvable on PATH
	if t.EngineBrowserToken != "" {
		// Least privilege, not a sandbox widening: this token is scoped
		// server-side to the browser tool only (internal/api's
		// browserScopedTools), so handing it to the harness does not grant
		// anything harnessEnv() otherwise withholds. Deliberately never the
		// full engine token — see the field doc on EngineBrowserToken.
		cmd.Env = append(cmd.Env,
			"KARMAX_API_URL="+t.EngineAPIURL,
			"KARMAX_API_TOKEN="+t.EngineBrowserToken,
		)
	}
	setupProcessGroup(cmd)

	done, unregister := registerRun(registryKey, workingDir, cancel)
	defer unregister()

	// cmd.Cancel replaces the default (which only kills cmd.Process) with a
	// whole-group kill — see terminateGroup. WaitDelay bounds how long
	// CombinedOutput can be blocked by a grandchild still holding the
	// output pipe open once Cancel has run; WaitDelay's own kill only
	// reaches cmd.Process, so it is terminateGroup's own SIGKILL, not
	// this, that finishes off the rest of the group.
	cmd.Cancel = func() error { return terminateGroup(cmd, done) }
	cmd.WaitDelay = 5 * time.Second

	return cmd.CombinedOutput()
}

// looksLikeUUID reports whether s is syntactically a session id the CLI
// would accept, as opposed to a caller's own stable SESSION KEY.
func looksLikeUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// looksLikeMissingSession reports whether a failed run looks like it hit a
// session the CLI has no record of — the exact signal a stale key -> uuid
// mapping produces on --resume.
func looksLikeMissingSession(output []byte) bool {
	return strings.Contains(string(output), "No conversation found")
}

// sessionCreated reports whether the CLI actually wrote a transcript for
// sessionID anywhere under ~/.claude/projects — proof a --session-id run
// minted a real, resumable session, rather than merely exiting 0.
//
// Found by matching the uuid's filename across every project directory,
// NOT by recomputing chatlog.Dir(workingDir) and checking that one exact
// path — confirmed live against the real CLI: it resolves symlinks in the
// working directory before deriving its own project-directory slug, so a
// working_dir under, say, macOS's /var/folders (a symlink to
// /private/var/folders) lands its transcript under a "-private-var-..."
// project directory, while chatlog.Slug(workingDir) computes "-var-..." from
// the unresolved path — a mismatch that would make this report "not
// created" for every successful run through a symlinked working directory,
// and a mapping that then never gets persisted. A session id is a uuid, so a
// filename collision across projects is not a real concern; matching on it
// alone is both simpler and correct regardless of how any particular
// working directory's symlinks resolve.
func sessionCreated(sessionID string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl"))
	return err == nil && len(matches) > 0
}

// Cleanup deletes a coding session's durable state: its transcript, its
// working directory, and its coding_sessions rows. The terminal moment in a
// non-ephemeral session's life — done, failed, a question that expired, or a
// daemon giving up tasks it can no longer hold — where nothing should ever
// resume into it again. Deleting a session that is already gone is not an
// error.
//
// workingDir must name a concrete, per-task directory. hostpaths.Resolve("")
// falls back to the SHARED hostpaths.WorkDir() root — the one directory every
// coding-tool call on the machine uses — which is exactly right for starting
// a task and exactly wrong here: os.RemoveAll on it would destroy the
// operator's whole workspace, not one session's. So an empty (or
// whitespace-only) workingDir, anything that resolves onto that shared root
// or a directory above it, and anything that resolves onto a bare top-level
// directory under it (the root's own "lyzn-tasks" folder, or an unrelated
// directory like "Documents") is refused outright — see resolveCleanupDir.
func (t *ClaudeCodeTool) Cleanup(workingDir, sessionID string) error {
	resolved, err := resolveCleanupDir(workingDir)
	if err != nil {
		return err
	}

	// sessionID may be a real CLI session id (unchanged behaviour) or a
	// caller's stable SESSION KEY — see run(). A key names no transcript by
	// itself, so it has to be resolved to the uuid it was mapped to before
	// the transcript can be found and removed.
	transcriptID := sessionID
	sessionKey := ""
	if sessionID != "" && !looksLikeUUID(sessionID) {
		sessionKey = sessionID
		transcriptID = ""
		if t.Store != nil {
			if mapped, err := t.Store.GetSessionKey(sessionKey); err == nil {
				transcriptID = mapped
			}
		}
	}

	if err := chatlog.RemoveSession(resolved, transcriptID); err != nil {
		return err
	}
	if err := os.RemoveAll(resolved); err != nil {
		return err
	}
	if t.Store == nil {
		return nil
	}
	// coding_sessions rows are keyed by whatever identity run() stored them
	// under — the session key when there is one, the CLI session id
	// otherwise — so deleting by sessionID here already matches either
	// scheme without needing to know which one it is.
	if sessionID != "" {
		if err := t.Store.DeleteCodingSessionsBySessionID(sessionID); err != nil {
			return err
		}
	}
	if sessionKey != "" {
		return t.Store.DeleteSessionKey(sessionKey)
	}
	return nil
}

// resolveCleanupDir is hostpaths.Resolve, plus the one guard Cleanup needs
// that no other caller does: require workingDir to resolve to a concrete
// per-task directory strictly beneath the shared hostpaths.WorkDir() root,
// rather than merely rejecting the root itself and directories above it. The
// difference matters: workingDir is externally supplied — a recipe's
// harness.forget, or a task id from LYZN by way of the six-hour sweep — so
// the only paths this may honour are ones that could not possibly be
// anything other than one task's own directory.
func resolveCleanupDir(workingDir string) (string, error) {
	root := hostpaths.WorkDir()
	if strings.TrimSpace(workingDir) == "" {
		return "", fmt.Errorf("claude_code: Cleanup refused an empty working_dir: it would resolve to the shared working directory %q and remove it entirely", root)
	}
	resolved := hostpaths.Resolve(workingDir)
	if !isConcreteTaskDir(resolved, root) {
		return "", fmt.Errorf("claude_code: Cleanup refused: working_dir %q resolves to %q, which is not a concrete task directory beneath the shared working directory %q", workingDir, resolved, root)
	}
	return resolved, nil
}

// isConcreteTaskDir reports whether resolved is a legitimate Cleanup target:
// strictly beneath root — never root itself, and never a path that escapes
// above it via ".." — and at least two path segments deep: a namespace
// directory (e.g. "lyzn-tasks") plus a concrete id beneath it.
//
// The depth requirement is not pedantry. Without it, "lyzn-tasks" itself —
// what an empty or trailing-slash task id renders `lyzn-tasks/{{ .id }}`
// down to — sits exactly one level under root, indistinguishable by
// ancestry alone from a real task directory, and deleting it deletes every
// task, including ones blocked awaiting an operator. The same one-level
// shape covers "Documents", which is a real directory of the operator's own
// home whenever hostpaths.WorkDir() has defaulted there.
func isConcreteTaskDir(resolved, root string) bool {
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	segments := strings.Split(filepath.ToSlash(rel), "/")
	if len(segments) < 2 {
		return false
	}
	for _, seg := range segments {
		if strings.TrimSpace(seg) == "" {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
