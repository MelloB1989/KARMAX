package main

import (
	"fmt"
	"os"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/spf13/cobra"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show, validate, or edit the configuration",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "show",
			Short: "Print the current configuration file",
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				data, err := os.ReadFile(findConfig())
				if err != nil {
					return fmt.Errorf("read config: %w", err)
				}
				fmt.Println(string(data))
				return nil
			},
		},
		&cobra.Command{
			Use:   "validate",
			Short: "Validate the configuration and print a summary",
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				cfg, err := config.Load(findConfig())
				if err != nil {
					return fmt.Errorf("config validation FAILED: %w", err)
				}
				fmt.Println("config validation OK")
				fmt.Printf("  agents: %d\n", len(cfg.Agents))
				fmt.Printf("  MCPs:   %d\n", len(cfg.MCPs))
				fmt.Printf("  routes: %d\n", len(cfg.Webhooks.Routes))
				fmt.Printf("  loops:  %d\n", len(cfg.Loops))
				return nil
			},
		},
		&cobra.Command{
			Use:   "edit",
			Short: "Print the command to open the config in $EDITOR",
			Args:  cobra.NoArgs,
			Run: func(_ *cobra.Command, _ []string) {
				editor := os.Getenv("EDITOR")
				if editor == "" {
					editor = "vi"
				}
				fmt.Printf("%s %s\n", editor, findConfig())
			},
		},
	)
	return cmd
}
