package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// These command groups print the API calls to run against a live instance
// (the daemon exposes everything over HTTP); they don't talk to it directly.

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Interact with agents (via the running instance's API)"}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List agents",
		Args:  cobra.NoArgs,
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println("Agent list requires a running karmax instance.")
			fmt.Println("Use the API: curl http://localhost:9091/api/activity")
		},
	})

	for _, action := range []string{"start", "stop", "pause", "resume", "restart"} {
		cmd.AddCommand(&cobra.Command{
			Use:   action + " <id>",
			Short: "Send " + action + " to an agent",
			Args:  cobra.ExactArgs(1),
			Run: func(_ *cobra.Command, args []string) {
				fmt.Printf("Sending %s to agent %s...\n", action, args[0])
				fmt.Printf("curl -X POST http://localhost:8080/api/agents/%s/%s\n", args[0], action)
			},
		})
	}

	var payload string
	trigger := &cobra.Command{
		Use:   "trigger <id>",
		Short: "Trigger an agent with a JSON payload",
		Args:  cobra.ExactArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			fmt.Printf("Triggering agent %s with payload: %s\n", args[0], payload)
			fmt.Printf("curl -X POST http://localhost:8080/api/agents/%s/trigger -d '%s'\n", args[0], payload)
		},
	}
	trigger.Flags().StringVar(&payload, "payload", "{}", "JSON payload")
	cmd.AddCommand(trigger)
	return cmd
}

func newSchedulerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "scheduler", Short: "Manage scheduled jobs"}
	cmd.AddCommand(
		&cobra.Command{Use: "list", Short: "List jobs", Args: cobra.NoArgs, Run: func(_ *cobra.Command, _ []string) {
			fmt.Println("curl http://localhost:8080/api/scheduler/jobs")
		}},
		&cobra.Command{Use: "add", Short: "Add a job", Args: cobra.NoArgs, Run: func(_ *cobra.Command, _ []string) {
			fmt.Println("usage: karmax scheduler add --name <name> --cron <expr> --agent <id>")
		}},
		&cobra.Command{Use: "remove <id>", Short: "Remove a job", Args: cobra.ExactArgs(1), Run: func(_ *cobra.Command, args []string) {
			fmt.Printf("curl -X DELETE http://localhost:8080/api/scheduler/jobs/%s\n", args[0])
		}},
		&cobra.Command{Use: "run <id>", Short: "Run a job immediately", Args: cobra.ExactArgs(1), Run: func(_ *cobra.Command, args []string) {
			fmt.Printf("curl -X POST http://localhost:8080/api/scheduler/jobs/%s/run\n", args[0])
		}},
	)
	return cmd
}

func newWebhookCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "webhook", Short: "List and test webhook routes"}
	cmd.AddCommand(&cobra.Command{Use: "list", Short: "List routes", Args: cobra.NoArgs, Run: func(_ *cobra.Command, _ []string) {
		fmt.Println("curl http://localhost:8080/api/webhooks/routes")
	}})
	var body string
	test := &cobra.Command{
		Use:   "test <path>",
		Short: "Test a webhook route",
		Args:  cobra.ExactArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			fmt.Printf("curl -X POST http://localhost:9090%s -H 'Content-Type: application/json' -d '%s'\n", args[0], body)
		},
	}
	test.Flags().StringVar(&body, "body", "{}", "JSON body")
	cmd.AddCommand(test)
	return cmd
}

func newMemoryCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "memory", Short: "Search and export agent memory"}
	var query string
	search := &cobra.Command{
		Use:   "search <namespace>",
		Short: "Search memory",
		Args:  cobra.ExactArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			fmt.Printf("curl 'http://localhost:8080/api/memory/%s/search?query=%s'\n", args[0], query)
		},
	}
	search.Flags().StringVar(&query, "query", "", "search query")
	export := &cobra.Command{
		Use:   "export <namespace>",
		Short: "Export memory",
		Args:  cobra.ExactArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			fmt.Printf("curl 'http://localhost:8080/api/memory/%s/recent?n=10000'\n", args[0])
		},
	}
	cmd.AddCommand(search, export)
	return cmd
}

func newToolCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tool", Short: "List and execute tools"}
	cmd.AddCommand(&cobra.Command{Use: "list", Short: "List tools", Args: cobra.NoArgs, Run: func(_ *cobra.Command, _ []string) {
		fmt.Println("curl http://localhost:8080/api/tools")
	}})
	var input string
	exec := &cobra.Command{
		Use:   "exec <name>",
		Short: "Execute a tool",
		Args:  cobra.ExactArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			fmt.Printf("curl -X POST http://localhost:8080/api/tools/%s/execute -d '%s'\n", args[0], input)
		},
	}
	exec.Flags().StringVar(&input, "input", "{}", "JSON input")
	cmd.AddCommand(exec)
	return cmd
}

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "mcp", Short: "Inspect MCP servers"}
	cmd.AddCommand(
		&cobra.Command{Use: "list", Short: "List MCP tools", Args: cobra.NoArgs, Run: func(_ *cobra.Command, _ []string) {
			fmt.Println("curl http://localhost:8080/api/tools")
			fmt.Println("(MCP tools are listed alongside built-in tools)")
		}},
		&cobra.Command{Use: "connect <server-id>", Short: "Connect to an MCP server", Args: cobra.ExactArgs(1), Run: func(_ *cobra.Command, args []string) {
			fmt.Printf("MCP connection to %s — configure in karmax.yaml under 'mcps'\n", args[0])
		}},
	)
	return cmd
}
