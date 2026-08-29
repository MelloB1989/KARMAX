package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// sessionCmd drives the harness supervisor by hand.
//
// A thin wrapper over the harness.* tools rather than a second implementation:
// the CLI, the workflows and the orchestrator all reach the supervisor through
// exactly one surface, so what an operator sees here is what production does.
func sessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Long-lived coding-harness conversations",
		Long: "Sessions are keyed by an arbitrary string. The same key continues the\n" +
			"same conversation; a new key starts a new one. A session that has gone\n" +
			"quiet is closed automatically, and one whose process died is resumed with\n" +
			"its context on the next message.",
	}

	var kind string
	send := &cobra.Command{
		Use:   "send <key> <message>",
		Short: "Send a message to a session, opening or resuming it as needed",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			text := args[1]
			for _, extra := range args[2:] {
				text += " " + extra
			}
			start := time.Now()
			if err := callTool("harness.send", map[string]any{
				"key": args[0], "kind": kind, "text": text,
			}, 5*time.Minute); err != nil {
				return err
			}
			// Printed because the whole design rests on the second turn being
			// fast; an operator should be able to see that without a stopwatch.
			fmt.Fprintf(c.ErrOrStderr(), "\n(%s)\n", time.Since(start).Round(time.Millisecond))
			return nil
		},
	}
	send.Flags().StringVar(&kind, "kind", "agent", "which configured policy to use (model, idle window, limits)")

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
