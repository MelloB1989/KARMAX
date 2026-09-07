package main

import (
	"time"

	"github.com/spf13/cobra"
)

// What KARMAX has been asked to do and has not finished.
//
// The same tools the orchestrator uses, so the list an operator sees at a
// terminal is the list the agent is actually working from.
func taskCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Work KARMAX owns until it is done",
		Long: "A task outlives the conversation that created it. KARMAX works each one in\n" +
			"its own Claude Code session — across restarts, retrying its own failures —\n" +
			"and reports back when it finishes or gets genuinely stuck.",
	}

	var status string
	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "Show what is outstanding",
		RunE: func(_ *cobra.Command, _ []string) error {
			return callTool("task.list", map[string]any{
				"status": status, "limit": limit,
			}, 30*time.Second)
		},
	}
	list.Flags().StringVar(&status, "status", "live",
		"live | open | working | blocked | done | failed | all")
	list.Flags().IntVar(&limit, "limit", 20, "how many to show")

	var title, workdir string
	var startIn int
	add := &cobra.Command{
		Use:   "add <goal...>",
		Short: "Hand KARMAX something to finish",
		Long: "The goal is all the session gets — it does not see this terminal or any\n" +
			"conversation. Say what must be true for the work to be done, and include\n" +
			"whatever it needs to get there.",
		Example: "  karmax task add 'upgrade the repo at ~/code/foo to Go 1.24 and make the tests pass'\n" +
			"  karmax task add --dir ~/code/KARMAX 'find and fix the flaky store test'",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			in := map[string]any{"goal": joinArgs(args)}
			if title != "" {
				in["title"] = title
			}
			if workdir != "" {
				abs, err := absPath(workdir)
				if err != nil {
					return err
				}
				in["workdir"] = abs
			}
			if startIn > 0 {
				in["start_in_minutes"] = startIn
			}
			return callTool("task.create", in, time.Minute)
		},
	}
	add.Flags().StringVar(&title, "title", "", "short label for status lists")
	add.Flags().StringVarP(&workdir, "dir", "C", "", "directory the work happens in")
	add.Flags().IntVar(&startIn, "start-in", 0, "wait this many minutes before the first round")

	var note string
	set := &cobra.Command{
		Use:   "set <task-id> <working|blocked|done|failed>",
		Short: "Close, cancel, or unblock a task",
		Long: "Setting a blocked task back to `working` is what resumes it: the runner\n" +
			"leaves blocked tasks alone until somebody answers what they were waiting on.",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			in := map[string]any{"task_id": args[0], "status": args[1]}
			if note != "" {
				in["note"] = note
			}
			return callTool("task.update", in, 30*time.Second)
		},
	}
	set.Flags().StringVar(&note, "note", "", "what changed, recorded for the next round to read")

	cmd.AddCommand(list, add, set)
	return cmd
}

func joinArgs(args []string) string {
	out := args[0]
	for _, a := range args[1:] {
		out += " " + a
	}
	return out
}
