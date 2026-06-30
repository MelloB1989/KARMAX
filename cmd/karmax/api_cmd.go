package main

import (
	"fmt"
	"net/url"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/spf13/cobra"
)

// These commands talk to a running KARMAX instance over its API (see
// apiclient.go), or read the config locally where that's the source of truth.

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Inspect configured agents"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List configured agents",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(findConfig())
			if err != nil {
				return err
			}
			if len(cfg.Agents) == 0 {
				fmt.Println("no agents configured.")
				return nil
			}
			for _, a := range cfg.Agents {
				fmt.Printf("• %-12s %s  (model: %s/%s, %d tools)\n", a.ID, a.Name, a.Provider, a.Model, len(a.Tools))
			}
			return nil
		},
	})
	return cmd
}

func newSchedulerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "scheduler", Short: "Inspect and run scheduled jobs / loops"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List scheduled jobs and loops",
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				out, err := apiGET("/api/activity")
				if err != nil {
					return err
				}
				jobs := asList(out["jobs"])
				if len(jobs) == 0 {
					fmt.Println("no scheduled jobs.")
					return nil
				}
				for _, j := range jobs {
					next := asStr(j["next_run"])
					if next == "" {
						next = "—"
					}
					fmt.Printf("• %-22s cron=%-14s runs=%v  next=%s\n", asStr(j["name"]), asStr(j["cron"]), j["run_count"], next)
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "run <id>",
			Short: "Run a job/loop now (e.g. loopkit:tech-news)",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				out, err := apiPOST("/api/jobs/" + url.PathEscape(args[0]) + "/run")
				if err != nil {
					return err
				}
				fmt.Printf("ran: %s\n", asStr(out["ran"]))
				return nil
			},
		},
	)
	return cmd
}

func newWebhookCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "webhook", Short: "Inspect webhook activity"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List recent webhook events",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			out, err := apiGET("/api/activity")
			if err != nil {
				return err
			}
			whs := asList(out["webhooks"])
			if len(whs) == 0 {
				fmt.Println("no recent webhook events.")
				return nil
			}
			for _, w := range whs {
				fmt.Printf("• %-6s %-24s %s\n", asStr(w["method"]), asStr(w["route"]), asStr(w["received_at"]))
			}
			return nil
		},
	})
	return cmd
}

func newMemoryCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "memory", Short: "Search and export agent memory"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "search <query>",
			Short: "Search long-term memory",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				out, err := apiGET("/api/memory/entries?limit=25&q=" + url.QueryEscape(args[0]))
				if err != nil {
					return err
				}
				entries := asList(out["entries"])
				if len(entries) == 0 {
					fmt.Println("no matching memory entries.")
					return nil
				}
				for _, e := range entries {
					fmt.Printf("• %s\n", oneLine(asStr(e["content"]), 200))
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "export",
			Short: "Dump recent memory entries",
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				out, err := apiGET("/api/memory/entries?limit=10000")
				if err != nil {
					return err
				}
				entries := asList(out["entries"])
				for _, e := range entries {
					fmt.Printf("- %s\n", oneLine(asStr(e["content"]), 400))
				}
				fmt.Printf("\n%d entries\n", len(entries))
				return nil
			},
		},
	)
	return cmd
}

func newToolCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tool", Short: "Inspect the agent's tools"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the tools available to the agent (from config)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(findConfig())
			if err != nil {
				return err
			}
			if len(cfg.Agents) == 0 || len(cfg.Agents[0].Tools) == 0 {
				fmt.Println("no tools configured.")
				return nil
			}
			for _, t := range cfg.Agents[0].Tools {
				fmt.Printf("• %s\n", t)
			}
			return nil
		},
	})
	return cmd
}

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "mcp", Short: "Inspect configured MCP servers"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List configured MCP servers (from config)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(findConfig())
			if err != nil {
				return err
			}
			if len(cfg.MCPs) == 0 {
				fmt.Println("no MCP servers configured.")
				return nil
			}
			for _, m := range cfg.MCPs {
				fmt.Printf("• %s\n", m.ID)
			}
			return nil
		},
	})
	return cmd
}
