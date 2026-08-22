package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/loopinstall"
	"github.com/MelloB1989/karmax/internal/runtime"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// redactDSN drops the password from a connection string, keeping the user.
//
// A DSN is operator-supplied config, and config gets printed — doctor, logs,
// error messages. The user is worth showing; the password never is.
func redactDSN(raw string) string {
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.User == nil {
			return raw
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.User(u.User.Username())
		}
		return u.String()
	}
	// go-sql-driver form: user:pass@tcp(host:port)/db
	if at := strings.Index(raw, "@"); at > 0 {
		if colon := strings.Index(raw[:at], ":"); colon >= 0 {
			return raw[:colon] + ":***" + raw[at:]
		}
	}
	return raw
}

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
			recordConfigPath(cfg.Karmax.DataDir, cfgFile)

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
		Short: "Show what every loop has actually been doing",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			res, err := apiGET("/api/loops/health")
			if err != nil {
				return fmt.Errorf("could not reach the daemon: %w\n"+
					"is it running? `systemctl --user status karmax`", err)
			}
			loops := asList(res["loops"])
			if len(loops) == 0 {
				fmt.Println("no loops registered")
				return nil
			}

			// Worst first — the server already sorts dark and failing to the
			// top, and a status page whose first line is fine is a status page
			// nobody finishes reading.
			fmt.Printf("%-20s %-9s %-16s %-8s %s\n", "LOOP", "STATE", "LAST SUCCESS", "FAILS", "NOTE")
			fmt.Println(strings.Repeat("─", 88))
			problems := 0
			for _, l := range loops {
				name := asStr(l["name"])
				state := "ok"
				switch {
				case boolOf(l["dark"]):
					state = "DARK"
					problems++
				case numOf(l["consecutive_failures"]) > 0:
					state = "failing"
					problems++
				case boolOf(l["running"]):
					state = "running"
				}
				note := ""
				if r := asStr(l["retry_at"]); r != "" {
					note = "retry " + humanTime(r)
				}
				if e := asStr(l["last_error"]); e != "" && note == "" {
					note = truncQ(oneLineStr(e), 40)
				}
				fmt.Printf("%-20s %-9s %-16s %-8d %s\n",
					truncQ(name, 20), state, humanTime(asStr(l["last_success"])),
					numOf(l["consecutive_failures"]), note)
			}
			fmt.Println()
			if problems == 0 {
				fmt.Printf("%d loops, all healthy\n", len(loops))
			} else {
				fmt.Printf("%d of %d loops need attention\n", problems, len(loops))
			}
			return nil
		},
	}
}

func boolOf(v any) bool { b, _ := v.(bool); return b }

func numOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// humanTime renders a timestamp as an age, because "3d ago" is the thing being
// judged and an RFC3339 string makes the reader do the subtraction.
func humanTime(s string) string {
	if strings.TrimSpace(s) == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	d := time.Since(t)
	if d < 0 {
		return "in " + shortDur(-d)
	}
	return shortDur(d) + " ago"
}

func shortDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
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
			cfg, cfgErr := config.Load(cfgFile)
			if cfgErr != nil {
				fmt.Printf("FAIL (%v)\n", cfgErr)
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

			probe := &config.KarmaxConfig{}
			probe.Karmax.DataDir = dataDir
			if cfgErr == nil {
				probe = cfg
			}
			dsn := probe.DatabaseDSN()
			if target, err := store.ParseDSN(dsn); err != nil {
				fmt.Printf("Database:    %s FAIL (%v)\n", redactDSN(dsn), err)
			} else if target.Kind == store.SQLite {
				path := target.Conn
				if i := strings.IndexByte(path, '?'); i >= 0 {
					path = path[:i]
				}
				fmt.Printf("SQLite DB:   %s ", path)
				info, statErr := os.Stat(path)
				// Opened, not just stat'd: a CGO_ENABLED=0 build carries a
				// SQLite driver that only fails when you use it, and a present
				// file said nothing about that.
				if err := store.Ping(dsn, 5*time.Second); err != nil {
					fmt.Printf("FAIL (%v)\n", err)
				} else if statErr == nil {
					fmt.Printf("OK (%d bytes)\n", info.Size())
				} else {
					fmt.Println("MISSING (will be created on first start)")
				}
			} else {
				fmt.Printf("Database:    %s (%s) ", redactDSN(dsn), target.Kind)
				if err := store.Ping(dsn, 5*time.Second); err != nil {
					fmt.Printf("FAIL (%v)\n", err)
				} else {
					fmt.Println("OK")
				}
			}

			for _, v := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "WHATSAPP_TARGET"} {
				if val := os.Getenv(v); val != "" {
					fmt.Printf("%-16s SET (%s...)\n", v+":", val[:min(8, len(val))])
				} else {
					fmt.Printf("%-16s NOT SET\n", v+":")
				}
			}

			fmt.Println()
			fmt.Println("Loops & build environment (for `karmax loops`):")
			checkLine("Go compiler", loopinstall.HaveGo(), "run `karmax setup` or install Go")
			checkLine("git", loopinstall.HaveGit(), "install git (needed to clone the source)")
			checkLine("C compiler (cgo)", commandOnPath("gcc") || commandOnPath("cc"), "apt install build-essential")
			if root, err := loopinstall.RepoRoot(); err == nil {
				fmt.Printf("  %-18s OK (%s)\n", "source checkout", root)
			} else {
				fmt.Printf("  %-18s MISSING (run `karmax setup`)\n", "source checkout")
			}
			fmt.Printf("  %-18s %d registered, %d disabled\n", "loops",
				len(loopkit.Registered()), len(loopinstall.LoadDisabledLoops()))
		},
	}
}

func checkLine(label string, ok bool, hint string) {
	if ok {
		fmt.Printf("  %-18s OK\n", label)
	} else {
		fmt.Printf("  %-18s MISSING — %s\n", label, hint)
	}
}
