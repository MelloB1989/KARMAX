package main

import (
	"fmt"
	"os"
	"strings"
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
			if err := callTool("harness.send", in, opt.timeout); err != nil {
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
		Use:   "session",
		Short: "Long-lived Claude Code conversations",
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
			if err := callTool("harness.send", in, opt.timeout); err != nil {
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

	list := &cobra.Command{
		Use:   "list",
		Short: "Show sessions, what they cost, and how much quota is left",
		RunE: func(c *cobra.Command, args []string) error {
			return callTool("harness.list", map[string]any{}, 30*time.Second)
		},
	}

	closeCmd := &cobra.Command{
		Use:   "close <key>",
		Short: "Close a session now instead of waiting for its idle window",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return callTool("harness.close", map[string]any{"key": args[0]}, 30*time.Second)
		},
	}

	cmd.AddCommand(send, list, closeCmd)
	return cmd
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
