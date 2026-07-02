package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// cfgPath is bound to the persistent --config flag and read by findConfig.
var cfgPath string

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "karmax",
		Short: "KARMAX — your always-on personal AI agent harness",
		Long: "KARMAX runs a personal AI agent (chat, WhatsApp, scheduled loops, long-term memory),\n" +
			"delegates heavy work to coding harnesses, and serves the companion phone app API.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&cfgPath, "config", "c", "",
		"path to karmax.yaml (default: ./karmax.yaml then ~/.karmax/karmax.yaml)")

	root.AddCommand(
		newStartCmd(),
		newOnboardCmd(),
		newInitCmd(),
		newSetupCmd(),
		newDoctorCmd(),
		newStatusCmd(),
		newAgentCmd(),
		newSchedulerCmd(),
		newWebhookCmd(),
		newMemoryCmd(),
		newToolCmd(),
		newMCPCmd(),
		newConfigCmd(),
		newLoopsCmd(),
		newAskCmd(),
		newNotifyCmd(),
		newSendCmd(),
	)
	return root
}

// Execute runs the root command and exits non-zero on error.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
