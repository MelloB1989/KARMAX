package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/MelloB1989/karmax/internal/config"
	_ "github.com/MelloB1989/karmax/internal/installedloops" // third-party loopkit loops (managed by `karmax loops`)
	_ "github.com/MelloB1989/karmax/internal/loops/core"     // built-in loopkit loops (migrated from karmax.yaml)
	"github.com/MelloB1989/karmax/internal/runtime"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
)

// loadDotEnv loads environment variables from a .env file (working directory
// first, then ~/.karmax/.env) so that ${VAR} references in karmax.yaml expand
// and provider SDKs pick up credentials. It is non-fatal and never overrides
// variables already present in the real environment.
func loadDotEnv() {
	_ = godotenv.Load()
	if home, err := os.UserHomeDir(); err == nil {
		_ = godotenv.Load(filepath.Join(home, ".karmax", ".env"))
	}
}

var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "start":
		runStart()
	case "init":
		runInit()
	case "doctor":
		runDoctor()
	case "status":
		runStatus()
	case "agent":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: karmax agent <list|start|stop|pause|resume|restart|logs|trigger|create|edit>")
			os.Exit(1)
		}
		runAgentCmd(os.Args[2:])
	case "scheduler":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: karmax scheduler <list|add|remove|run>")
			os.Exit(1)
		}
		runSchedulerCmd(os.Args[2:])
	case "webhook":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: karmax webhook <list|test>")
			os.Exit(1)
		}
		runWebhookCmd(os.Args[2:])
	case "memory":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: karmax memory <search|export>")
			os.Exit(1)
		}
		runMemoryCmd(os.Args[2:])
	case "tool":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: karmax tool <list|exec>")
			os.Exit(1)
		}
		runToolCmd(os.Args[2:])
	case "loops":
		runLoopsCmd(os.Args[2:])
	case "mcp":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: karmax mcp <list|connect>")
			os.Exit(1)
		}
		runMCPCmd(os.Args[2:])
	case "config":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: karmax config <show|validate|edit>")
			os.Exit(1)
		}
		runConfigCmd(os.Args[2:])
	case "version":
		fmt.Printf("karmax %s\n", Version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`karmax %s — AI Agent Harness

Usage:
  karmax <command> [options]

Commands:
  start                Start the runtime (agents + webhook + scheduler + phone API)
  init                 Interactive first-run setup wizard
  doctor               Check environment, config, and connections
  status               Show all agent statuses
  version              Print version

  agent list           List all agents
  agent start <id>     Start a specific agent
  agent stop <id>      Stop a specific agent
  agent pause <id>     Pause a specific agent
  agent resume <id>    Resume a specific agent
  agent restart <id>   Restart a specific agent
  agent trigger <id>   Trigger an agent with a payload

  scheduler list       List scheduled jobs
  scheduler add        Add a scheduled job
  scheduler remove <id>  Remove a scheduled job
  scheduler run <id>   Run a job immediately

  webhook list         List webhook routes
  webhook test <path>  Test a webhook route

  memory search <ns>   Search agent memory
  memory export <ns>   Export memory to markdown

  tool list            List registered tools
  tool exec <name>     Execute a tool

  mcp list             List MCP servers
  config show          Show current configuration
  config validate      Validate configuration

`, Version)
}

func findConfig() string {
	candidates := []string{
		"karmax.yaml",
		"karmax.yml",
	}

	home, _ := os.UserHomeDir()
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".karmax", "karmax.yaml"),
			filepath.Join(home, ".karmax", "karmax.yml"),
		)
	}

	for i, c := range os.Args {
		if (c == "--config" || c == "-c") && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}

	return "karmax.yaml"
}

func runStart() {
	loadDotEnv()
	cfgPath := findConfig()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config from %s: %v\n", cfgPath, err)
		fmt.Fprintln(os.Stderr, "Run 'karmax init' to create a configuration file.")
		os.Exit(1)
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
		log.Fatal("runtime init failed", zap.Error(err))
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
		log.Fatal("runtime error", zap.Error(err))
	}

	fmt.Println("karmax stopped")
}

func runInit() {
	fmt.Println()
	fmt.Println("  ██╗  ██╗ █████╗ ██████╗ ███╗   ███╗ █████╗ ██╗  ██╗")
	fmt.Println("  ██║ ██╔╝██╔══██╗██╔══██╗████╗ ████║██╔══██╗╚██╗██╔╝")
	fmt.Println("  █████╔╝ ███████║██████╔╝██╔████╔██║███████║ ╚███╔╝ ")
	fmt.Println("  ██╔═██╗ ██╔══██║██╔══██╗██║╚██╔╝██║██╔══██║ ██╔██╗ ")
	fmt.Println("  ██║  ██╗██║  ██║██║  ██║██║ ╚═╝ ██║██║  ██║██╔╝ ██╗")
	fmt.Println("  ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝     ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝")
	fmt.Println()
	fmt.Println("  AI Agent Harness — First Run Setup")
	fmt.Println()

	home, _ := os.UserHomeDir()
	dataDir := filepath.Join(home, ".karmax")
	cfgPath := filepath.Join(dataDir, "karmax.yaml")

	os.MkdirAll(dataDir, 0755)
	os.MkdirAll(filepath.Join(dataDir, "memory"), 0755)
	os.MkdirAll(filepath.Join(dataDir, "db"), 0755)

	if err := config.SaveDefault(cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("  + Created %s/\n", dataDir)
	fmt.Printf("  + Created %s\n", cfgPath)
	fmt.Printf("  + Created %s/memory/\n", dataDir)
	fmt.Printf("  + Created %s/db/\n", dataDir)
	fmt.Println("  + Added sample agent: \"hello-world\"")
	fmt.Println()
	fmt.Println("  Set your API key:")
	fmt.Println("    export OPENAI_API_KEY=sk-...")
	fmt.Println()
	fmt.Println("  Run with:  karmax start")
	fmt.Println("  Phone app API: http://localhost:9091")
	fmt.Println()
}

func runDoctor() {
	loadDotEnv()
	fmt.Println("karmax doctor")
	fmt.Println("------------")

	cfgPath := findConfig()
	fmt.Printf("Config file: %s ", cfgPath)
	_, err := config.Load(cfgPath)
	if err != nil {
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

	envVars := []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "WHATSAPP_TARGET"}
	for _, v := range envVars {
		val := os.Getenv(v)
		if val != "" {
			fmt.Printf("%-16s SET (%s...)\n", v+":", val[:min(8, len(val))])
		} else {
			fmt.Printf("%-16s NOT SET\n", v+":")
		}
	}
}

func runStatus() {
	fmt.Println("Agent Status")
	fmt.Println("(Note: full status requires a running karmax instance)")
	fmt.Println("Run 'karmax start', then use the phone app or the API on :9091.")
}

func runAgentCmd(args []string) {
	switch args[0] {
	case "list":
		fmt.Println("Agent list requires a running karmax instance.")
		fmt.Println("Use the API: curl http://localhost:9091/api/activity")
	case "start", "stop", "pause", "resume", "restart":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "usage: karmax agent %s <id>\n", args[0])
			os.Exit(1)
		}
		fmt.Printf("Sending %s to agent %s...\n", args[0], args[1])
		fmt.Printf("curl -X POST http://localhost:8080/api/agents/%s/%s\n", args[1], args[0])
	case "trigger":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: karmax agent trigger <id> [--payload '{...}']")
			os.Exit(1)
		}
		payload := "{}"
		for i, a := range args {
			if a == "--payload" && i+1 < len(args) {
				payload = args[i+1]
			}
		}
		fmt.Printf("Triggering agent %s with payload: %s\n", args[1], payload)
		fmt.Printf("curl -X POST http://localhost:8080/api/agents/%s/trigger -d '%s'\n", args[1], payload)
	default:
		fmt.Fprintf(os.Stderr, "unknown agent command: %s\n", args[0])
	}
}

func runSchedulerCmd(args []string) {
	switch args[0] {
	case "list":
		fmt.Println("curl http://localhost:8080/api/scheduler/jobs")
	case "add":
		fmt.Println("usage: karmax scheduler add --name <name> --cron <expr> --agent <id>")
	case "remove":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: karmax scheduler remove <id>")
			os.Exit(1)
		}
		fmt.Printf("curl -X DELETE http://localhost:8080/api/scheduler/jobs/%s\n", args[1])
	case "run":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: karmax scheduler run <id>")
			os.Exit(1)
		}
		fmt.Printf("curl -X POST http://localhost:8080/api/scheduler/jobs/%s/run\n", args[1])
	default:
		fmt.Fprintf(os.Stderr, "unknown scheduler command: %s\n", args[0])
	}
}

func runWebhookCmd(args []string) {
	switch args[0] {
	case "list":
		fmt.Println("curl http://localhost:8080/api/webhooks/routes")
	case "test":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: karmax webhook test <path> [--body '{...}']")
			os.Exit(1)
		}
		body := "{}"
		for i, a := range args {
			if a == "--body" && i+1 < len(args) {
				body = args[i+1]
			}
		}
		port := "9090"
		fmt.Printf("curl -X POST http://localhost:%s%s -H 'Content-Type: application/json' -d '%s'\n", port, args[1], body)
	default:
		fmt.Fprintf(os.Stderr, "unknown webhook command: %s\n", args[0])
	}
}

func runMemoryCmd(args []string) {
	switch args[0] {
	case "search":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: karmax memory search <namespace> --query <query>")
			os.Exit(1)
		}
		query := ""
		for i, a := range args {
			if a == "--query" && i+1 < len(args) {
				query = args[i+1]
			}
		}
		fmt.Printf("curl 'http://localhost:8080/api/memory/%s/search?query=%s'\n", args[1], query)
	case "export":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: karmax memory export <namespace> [--out <file>]")
			os.Exit(1)
		}
		fmt.Printf("curl 'http://localhost:8080/api/memory/%s/recent?n=10000'\n", args[1])
	default:
		fmt.Fprintf(os.Stderr, "unknown memory command: %s\n", args[0])
	}
}

func runToolCmd(args []string) {
	switch args[0] {
	case "list":
		fmt.Println("curl http://localhost:8080/api/tools")
	case "exec":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: karmax tool exec <name> --input '{...}'")
			os.Exit(1)
		}
		input := "{}"
		for i, a := range args {
			if a == "--input" && i+1 < len(args) {
				input = args[i+1]
			}
		}
		fmt.Printf("curl -X POST http://localhost:8080/api/tools/%s/execute -d '%s'\n", args[1], input)
	default:
		fmt.Fprintf(os.Stderr, "unknown tool command: %s\n", args[0])
	}
}

func runMCPCmd(args []string) {
	switch args[0] {
	case "list":
		fmt.Println("curl http://localhost:8080/api/tools")
		fmt.Println("(MCP tools are listed alongside built-in tools)")
	case "connect":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: karmax mcp connect <server-id>")
			os.Exit(1)
		}
		fmt.Printf("MCP connection to %s — configure in karmax.yaml under 'mcps'\n", args[1])
	default:
		fmt.Fprintf(os.Stderr, "unknown mcp command: %s\n", args[0])
	}
}

func runConfigCmd(args []string) {
	switch args[0] {
	case "show":
		cfgPath := findConfig()
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read config: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
	case "validate":
		cfgPath := findConfig()
		cfg, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "config validation FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("config validation OK")
		fmt.Printf("  agents: %d\n", len(cfg.Agents))
		fmt.Printf("  MCPs:   %d\n", len(cfg.MCPs))
		fmt.Printf("  routes: %d\n", len(cfg.Webhooks.Routes))
		fmt.Printf("  loops:  %d\n", len(cfg.Loops))
	case "edit":
		cfgPath := findConfig()
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		fmt.Printf("Opening %s in %s...\n", cfgPath, editor)
		fmt.Printf("%s %s\n", editor, cfgPath)
	default:
		fmt.Fprintf(os.Stderr, "unknown config command: %s\n", args[0])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Suppress unused import warnings
var _ = json.Marshal
var _ = strings.TrimSpace
