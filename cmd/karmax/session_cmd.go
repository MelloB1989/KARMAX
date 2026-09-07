package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// Driving Claude Code from the terminal.
//
// A thin wrapper over the harness.* tools rather than a second implementation:
// the CLI, the workflows and the orchestrator all reach the supervisor through
// exactly one surface, so what an operator sees here is what production does.

// sessionOptions are the knobs shared by `karmax claude` and `karmax session
// send`, so the two cannot drift into different capabilities.
type sessionOptions struct {
	model       string
	kind        string
	dir         string
	context     string
	contextFile string
	promptFile  string
	closeAfter  bool
	timeout     time.Duration
}

func (o *sessionOptions) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVarP(&o.model, "model", "m", "",
		"haiku | sonnet | opus | fable, or a full model id (default: the kind's model)")
	f.StringVar(&o.kind, "kind", "",
		"policy to use — idle window, turn timeout, default model (default: agent)")
	f.StringVarP(&o.dir, "dir", "C", "",
		"directory the session runs in; it can read and write there")
	f.StringVar(&o.context, "context", "",
		"standing brief for the session, written to CLAUDE.md in its directory")
	f.StringVar(&o.contextFile, "context-file", "",
		"read the standing brief from a file")
	f.StringVarP(&o.promptFile, "file", "f", "",
		"read the prompt from a file instead of the arguments")
	f.BoolVar(&o.closeAfter, "close", false,
		"close the session once this turn finishes, instead of leaving it warm")
	f.DurationVar(&o.timeout, "timeout", 10*time.Minute,
		"how long to wait for the reply")
}

// input assembles the tool call from the flags and the positional arguments.
func (o *sessionOptions) input(key string, args []string) (map[string]any, error) {
	prompt := strings.Join(args, " ")
	if o.promptFile != "" {
		b, err := os.ReadFile(o.promptFile)
		if err != nil {
			return nil, fmt.Errorf("reading the prompt: %w", err)
		}
		prompt = strings.TrimSpace(string(b))
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("nothing to send — give a prompt, or --file")
	}

	brief := o.context
	if o.contextFile != "" {
		b, err := os.ReadFile(o.contextFile)
		if err != nil {
			return nil, fmt.Errorf("reading the context: %w", err)
		}
		brief = string(b)
	}

	kind := o.kind
	if kind == "" {
		kind = "agent"
	}
	in := map[string]any{"key": key, "kind": kind, "text": prompt}
	if o.model != "" {
		in["model"] = o.model
	}
	if o.dir != "" {
		abs, err := absPath(o.dir)
		if err != nil {
			return nil, err
		}
		in["workdir"] = abs
	}
	if strings.TrimSpace(brief) != "" {
		in["instructions"] = brief
	}
	return in, nil
}

// claudeCmd is the one-liner: ask Claude Code something, from anywhere.
func claudeCmd() *cobra.Command {
	var opt sessionOptions
	var key string
	var fresh bool

	cmd := &cobra.Command{
		Use:   "claude [prompt...]",
		Short: "Ask Claude Code, in a session that stays warm between calls",
		Long: "Runs a prompt through a long-lived Claude Code session — the same engine\n" +
			"KARMAX thinks with, with a real shell and every karmax tool a call away.\n\n" +
			"By default every invocation continues ONE operator session, so the second\n" +
			"question and the third are answered in about a second and a half instead of\n" +
			"paying a cold start each time. Use --key to keep separate conversations, or\n" +
			"--new to start a fresh one.\n\n" +
			"Pick the model for the job: haiku to classify or summarise, sonnet for\n" +
			"ordinary work, opus for hard reasoning, fable for the hardest.",
		Example: "  karmax claude 'what did I commit yesterday?'\n" +
			"  karmax claude -m opus 'work out why the loop retries forever'\n" +
			"  karmax claude -C ~/code/KARMAX -m sonnet 'run the tests and summarise failures'\n" +
			"  karmax claude --key refactor --context-file brief.md 'start on step 1'\n" +
			"  karmax claude -m haiku --close 'summarise this' -f notes.txt",
		Args: cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			switch {
			case fresh:
				key = "cli:" + uuid.New().String()[:8]
			case key == "":
				// One warm operator session by default. A key per invocation
				// would pay ~12.7k tokens of session overhead every time, which
				// is the cost this whole design exists to amortise.
				key = "cli:operator"
			}
			in, err := opt.input(key, args)
			if err != nil {
				return err
			}
			start := time.Now()
			if err := callToolField("harness.send", "reply", in, opt.timeout); err != nil {
				return err
			}
			fmt.Fprintf(c.ErrOrStderr(), "\n(%s · session %s)\n",
				time.Since(start).Round(time.Millisecond), key)
			if opt.closeAfter {
				return callTool("harness.close", map[string]any{"key": key}, 30*time.Second)
			}
			return nil
		},
	}
	opt.bind(cmd)
	cmd.Flags().StringVarP(&key, "key", "k", "",
		"conversation to continue (default: one shared operator session)")
	cmd.Flags().BoolVar(&fresh, "new", false, "start a fresh conversation rather than continuing one")
	return cmd
}

// sessionCmd manages the sessions themselves.
func sessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "session",
		Aliases: []string{"sessions"},
		Short:   "Long-lived Claude Code conversations",
		Long: "Sessions are keyed by an arbitrary string. The same key continues the\n" +
			"same conversation; a new key starts a new one. A session that has gone\n" +
			"quiet is closed automatically, and one whose process died is resumed with\n" +
			"its context on the next message.\n\n" +
			"For everyday use, `karmax claude` is the shorter way in.",
	}

	var opt sessionOptions
	send := &cobra.Command{
		Use:   "send <key> <message...>",
		Short: "Send a message to a session, opening or resuming it as needed",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			in, err := opt.input(args[0], args[1:])
			if err != nil {
				return err
			}
			start := time.Now()
			if err := callToolField("harness.send", "reply", in, opt.timeout); err != nil {
				return err
			}
			// Printed because the whole design rests on the second turn being
			// fast; an operator should be able to see that without a stopwatch.
			fmt.Fprintf(c.ErrOrStderr(), "\n(%s)\n", time.Since(start).Round(time.Millisecond))
			if opt.closeAfter {
				return callTool("harness.close", map[string]any{"key": args[0]}, 30*time.Second)
			}
			return nil
		},
	}
	opt.bind(send)

	var showAll bool
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "Show sessions, what they cost, and how much quota is left",
		RunE: func(c *cobra.Command, _ []string) error {
			return printSessions(c, showAll)
		},
	}
	list.Flags().BoolVarP(&showAll, "all", "a", false, "include sessions that have already ended")

	show := &cobra.Command{
		Use:   "show <key>",
		Short: "Everything about one session",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return callTool("harness.show", map[string]any{"key": args[0]}, 30*time.Second)
		},
	}

	var tail int
	transcript := &cobra.Command{
		Use:     "transcript <key>",
		Aliases: []string{"log", "logs"},
		Short:   "Read what a session actually said and did",
		Long: "The conversation itself, including which tools each turn ran. This is how\n" +
			"to find out what a long-running task has been doing, rather than inferring\n" +
			"it from a status column.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return printTranscript(c, args[0], tail)
		},
	}
	transcript.Flags().IntVarP(&tail, "tail", "n", 20, "how many recent exchanges to show")

	model := &cobra.Command{
		Use:   "model <key> <haiku|sonnet|opus|fable>",
		Short: "Move a session to a different model",
		Long: "A model is chosen when the process starts, so this closes the running one.\n" +
			"The next message resumes the same conversation on the new tier, with its\n" +
			"context intact.",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return callTool("harness.model",
				map[string]any{"key": args[0], "model": args[1]}, 30*time.Second)
		},
	}

	var closeAll bool
	closeCmd := &cobra.Command{
		Use:   "close [key]",
		Short: "Close a session now instead of waiting for its idle window",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if closeAll {
				return closeEverySession(c)
			}
			if len(args) == 0 {
				return fmt.Errorf("name a session to close, or pass --all")
			}
			return callTool("harness.close", map[string]any{"key": args[0]}, 30*time.Second)
		},
	}
	closeCmd.Flags().BoolVarP(&closeAll, "all", "a", false, "close every live session")

	var olderThan time.Duration
	prune := &cobra.Command{
		Use:   "prune",
		Short: "Forget sessions that have already ended",
		Long: "Only closed and dead ones. A row is what makes a transcript reachable, so\n" +
			"a live or idle session is never dropped.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return callTool("harness.prune",
				map[string]any{"older_than_hours": olderThan.Hours()}, 30*time.Second)
		},
	}
	prune.Flags().DurationVar(&olderThan, "older-than", 7*24*time.Hour,
		"only sessions idle at least this long")

	cmd.AddCommand(send, list, show, transcript, model, closeCmd, prune)
	return cmd
}

// printSessions renders the table an operator actually reads.
//
// A JSON dump was what this printed before, which is fine for a machine and
// means an operator has to scan braces to answer "what is running and what is
// it costing me".
func printSessions(c *cobra.Command, all bool) error {
	out, err := apiPOSTJSON("/api/tools/"+url.PathEscape("harness.list"), map[string]any{}, 30*time.Second)
	if err != nil {
		return err
	}
	if ok, _ := out["ok"].(bool); !ok {
		return fmt.Errorf("harness.list: %s", asStr(out["error"]))
	}
	body, _ := out["output"].(map[string]any)
	if enabled, present := body["enabled"].(bool); present && !enabled {
		fmt.Println("Claude Code is not enabled (set harness.enabled in karmax.yaml).")
		return nil
	}

	rows, _ := body["sessions"].([]any)
	w := tabwriter.NewWriter(c.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tKIND\tMODEL\tSTATE\tTURNS\tCOST\tIDLE")
	shown := 0
	for _, r := range rows {
		m, _ := r.(map[string]any)
		state := asStr(m["state"])
		if !all && (state == "closed" || state == "dead") {
			continue
		}
		if live, _ := m["live"].(bool); live {
			state = "live"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t$%.2f\t%s\n",
			asStr(m["key"]), asStr(m["kind"]), asStr(m["model"]), state,
			asStr(m["turns"]), asFloat(m["cost_usd"]), asStr(m["idle_for"]))
		shown++
	}
	if shown == 0 {
		fmt.Fprintln(c.OutOrStdout(), "No sessions. One opens the moment anything needs to think.")
	} else if err := w.Flush(); err != nil {
		return err
	}

	// The quota line, because it is the number that decides which model
	// everything runs on next.
	if limits, ok := body["rate_limits"].(map[string]any); ok && len(limits) > 0 {
		fmt.Fprintln(c.OutOrStdout())
		for name, v := range limits {
			win, _ := v.(map[string]any)
			fmt.Fprintf(c.OutOrStdout(), "  %-10s %s used, resets %s\n",
				name, asStr(win["used"]), asStr(win["resets_at"]))
		}
	}
	if paused, _ := body["paused"].(bool); paused {
		fmt.Fprintf(c.OutOrStdout(), "\n  %s\n", asStr(body["pause_reason"]))
	}
	return nil
}

// printTranscript renders a conversation as a conversation.
func printTranscript(c *cobra.Command, key string, tail int) error {
	out, err := apiPOSTJSON("/api/tools/"+url.PathEscape("harness.transcript"),
		map[string]any{"key": key, "tail": tail}, time.Minute)
	if err != nil {
		return err
	}
	if ok, _ := out["ok"].(bool); !ok {
		return fmt.Errorf("%s", asStr(out["error"]))
	}
	body, _ := out["output"].(map[string]any)
	entries, _ := body["entries"].([]any)
	if len(entries) == 0 {
		fmt.Fprintln(c.OutOrStdout(), "Nothing in this session's transcript yet.")
		return nil
	}
	for _, e := range entries {
		m, _ := e.(map[string]any)
		role := asStr(m["role"])
		fmt.Fprintf(c.OutOrStdout(), "\n\u001b[1m%s\u001b[0m  %s\n", role, asStr(m["at"]))
		if txt := asStr(m["text"]); txt != "" {
			fmt.Fprintln(c.OutOrStdout(), "  "+strings.ReplaceAll(txt, "\n", "\n  "))
		}
		if used, ok := m["tools"].([]any); ok && len(used) > 0 {
			names := make([]string, 0, len(used))
			for _, u := range used {
				names = append(names, asStr(u))
			}
			fmt.Fprintf(c.OutOrStdout(), "  → %s\n", strings.Join(names, ", "))
		}
	}
	return nil
}

// closeEverySession stops everything that is running.
func closeEverySession(c *cobra.Command) error {
	out, err := apiPOSTJSON("/api/tools/"+url.PathEscape("harness.list"), map[string]any{}, 30*time.Second)
	if err != nil {
		return err
	}
	body, _ := out["output"].(map[string]any)
	rows, _ := body["sessions"].([]any)
	closed := 0
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if live, _ := m["live"].(bool); !live {
			continue
		}
		key := asStr(m["key"])
		// Called directly rather than through callTool: printing each close's
		// JSON on the way to a one-line summary is noise, not output.
		if _, err := apiPOSTJSON("/api/tools/"+url.PathEscape("harness.close"),
			map[string]any{"key": key}, 30*time.Second); err != nil {
			fmt.Fprintf(c.ErrOrStderr(), "  could not close %s: %v\n", key, err)
			continue
		}
		closed++
	}
	fmt.Fprintf(c.OutOrStdout(), "Closed %d live session(s).\n", closed)
	return nil
}

func asFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

// absPath resolves a directory the session will run in, so a relative path
// means what the operator's shell means by it rather than what the daemon does.
func absPath(dir string) (string, error) {
	if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = home + dir[1:]
	}
	if strings.HasPrefix(dir, "/") {
		return dir, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return wd + "/" + dir, nil
}
