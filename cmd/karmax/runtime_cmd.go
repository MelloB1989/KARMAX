package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/runtime"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "start",
		Short:         "Start the runtime (agents + webhooks + scheduler + phone API)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			loadDotEnv()
			cfgFile := findConfig()
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("load config from %s: %w\nRun 'karmax init' to create one", cfgFile, err)
			}

			var log *zap.Logger
			if cfg.Karmax.LogFormat == "pretty" {
				log, _ = zap.NewDevelopment()
			} else {
				log, _ = zap.NewProduction()
			}
			defer log.Sync()

			rt, err := runtime.New(cfg, log)
			if err != nil {
				return fmt.Errorf("runtime init: %w", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sigs := make(chan os.Signal, 1)
			signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-sigs
				fmt.Println("\nshutdown signal received")
				cancel()
			}()

			if err := rt.Start(ctx); err != nil {
				return fmt.Errorf("runtime: %w", err)
			}
			fmt.Println("karmax stopped")
			return nil
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show agent status (requires a running instance)",
		Args:  cobra.NoArgs,
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println("Agent Status")
			fmt.Println("(Note: full status requires a running karmax instance)")
			fmt.Println("Run 'karmax start', then use the phone app or the API on :9091.")
		},
	}
}

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Interactive first-run setup (creates ~/.karmax)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			banner := []string{
				"",
				"  ██╗  ██╗ █████╗ ██████╗ ███╗   ███╗ █████╗ ██╗  ██╗",
				"  ██║ ██╔╝██╔══██╗██╔══██╗████╗ ████║██╔══██╗╚██╗██╔╝",
				"  █████╔╝ ███████║██████╔╝██╔████╔██║███████║ ╚███╔╝ ",
				"  ██╔═██╗ ██╔══██║██╔══██╗██║╚██╔╝██║██╔══██║ ██╔██╗ ",
				"  ██║  ██╗██║  ██║██║  ██║██║ ╚═╝ ██║██║  ██║██╔╝ ██╗",
				"  ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝     ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝",
				"",
				"  AI Agent Harness — First Run Setup",
				"",
			}
			for _, l := range banner {
				fmt.Println(l)
			}

			home, _ := os.UserHomeDir()
			dataDir := filepath.Join(home, ".karmax")
			cfgFile := filepath.Join(dataDir, "karmax.yaml")
			os.MkdirAll(dataDir, 0755)
			os.MkdirAll(filepath.Join(dataDir, "memory"), 0755)
			os.MkdirAll(filepath.Join(dataDir, "db"), 0755)

			if err := config.SaveDefault(cfgFile); err != nil {
				return fmt.Errorf("create config: %w", err)
			}

			fmt.Printf("  + Created %s/\n", dataDir)
			fmt.Printf("  + Created %s\n", cfgFile)
			fmt.Printf("  + Created %s/memory/\n", dataDir)
			fmt.Printf("  + Created %s/db/\n", dataDir)
			fmt.Println("  + Added sample agent: \"hello-world\"")
			fmt.Println()
			fmt.Println("  Run with:  karmax start")
			fmt.Println("  Phone app API: http://localhost:9091")
			fmt.Println()
			return nil
		},
	}
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check environment, config, and connections",
		Args:  cobra.NoArgs,
		Run: func(_ *cobra.Command, _ []string) {
			loadDotEnv()
			fmt.Println("karmax doctor")
			fmt.Println("------------")

			cfgFile := findConfig()
			fmt.Printf("Config file: %s ", cfgFile)
			if _, err := config.Load(cfgFile); err != nil {
				fmt.Printf("FAIL (%v)\n", err)
			} else {
				fmt.Println("OK")
			}

			home, _ := os.UserHomeDir()
			dataDir := filepath.Join(home, ".karmax")
			fmt.Printf("Data dir:    %s ", dataDir)
			if info, err := os.Stat(dataDir); err == nil && info.IsDir() {
				fmt.Println("OK")
			} else {
				fmt.Println("MISSING (run 'karmax init')")
			}

			dbPath := filepath.Join(dataDir, "db", "karmax.db")
			fmt.Printf("SQLite DB:   %s ", dbPath)
			if _, err := os.Stat(dbPath); err == nil {
				fmt.Println("OK")
			} else {
				fmt.Println("MISSING (will be created on first start)")
			}

			for _, v := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "WHATSAPP_TARGET"} {
				if val := os.Getenv(v); val != "" {
					fmt.Printf("%-16s SET (%s...)\n", v+":", val[:min(8, len(val))])
				} else {
					fmt.Printf("%-16s NOT SET\n", v+":")
				}
			}
		},
	}
}
