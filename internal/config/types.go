package config

import (
	"github.com/MelloB1989/karmax/internal/mcp"
)

type KarmaxConfig struct {
	Karmax   KarmaxCoreConfig      `yaml:"karmax"`
	Webhooks WebhooksConfig        `yaml:"webhooks"`
	API      APIConfig             `yaml:"api"`
	AI       AIConfig              `yaml:"ai"`
	MCPs     []mcp.MCPServerConfig `yaml:"mcps"`
	Comms    CommsConfig           `yaml:"comms"`
	Agents   []AgentDefConfig      `yaml:"agents"`
	Loops    []LoopConfig          `yaml:"loops"`
	ColdScan ColdScanConfig        `yaml:"cold_scan"`
}

// ColdScanConfig controls the background "cold" memory worker that summarizes
// older WhatsApp chats into chat_summaries for the retrieval sub-agent.
type ColdScanConfig struct {
	Enabled          bool    `yaml:"enabled"`
	IntervalMinutes  int     `yaml:"interval_minutes"`    // default 20
	PerTick          int     `yaml:"per_tick"`            // chats per tick, default 4
	HotDays          int     `yaml:"hot_days"`            // active window, default 14
	MinGroupOwn      int     `yaml:"min_group_own"`       // min own msgs for a group, default 5
	MinGroupOwnRatio float64 `yaml:"min_group_own_ratio"` // min own-message fraction for a group, default 0.2
	WacliPath        string  `yaml:"wacli_path"`
}

// LoopConfig declares a recurring trigger that fires a prompt to an agent on a
// schedule. The agent decides what to do based on the prompt. Use `every` for
// simple intervals ("30m", "6h") or `cron` for a specific time
// (sec min hour dom mon dow).
type LoopConfig struct {
	Name    string         `yaml:"name"`
	Cron    string         `yaml:"cron"`
	Every   string         `yaml:"every"`
	Prompt  string         `yaml:"prompt"`
	Agent   string         `yaml:"agent"`   // defaults to the first agent
	Enabled *bool          `yaml:"enabled"` // defaults to true
	Payload map[string]any `yaml:"payload"` // optional extra context for the event
}

type CommsConfig struct {
	Channels []ChannelConfig `yaml:"channels"`
}

type ChannelConfig struct {
	ID       string            `yaml:"id"`
	Type     string            `yaml:"type"`
	AgentID  string            `yaml:"agent_id"`
	Token    string            `yaml:"token"`
	Settings map[string]string `yaml:"settings"`
}

type KarmaxCoreConfig struct {
	Version   string `yaml:"version"`
	DataDir   string `yaml:"data_dir"`
	LogLevel  string `yaml:"log_level"`
	LogFormat string `yaml:"log_format"`
}

type WebhooksConfig struct {
	Enabled bool                 `yaml:"enabled"`
	Port    int                  `yaml:"port"`
	Host    string               `yaml:"host"`
	Routes  []WebhookRouteConfig `yaml:"routes"`
}

// APIConfig configures the HTTP API the phone app talks to. It binds to
// 0.0.0.0 so it is reachable over both the LAN and Tailscale. Auth is via the
// KARMAX_API_TOKEN environment variable.
type APIConfig struct {
	Enabled bool   `yaml:"enabled"`
	Port    int    `yaml:"port"`
	Host    string `yaml:"host"`
}

type WebhookRouteConfig struct {
	Path            string         `yaml:"path"`
	Method          string         `yaml:"method"`
	AgentID         string         `yaml:"agent_id"`
	BusEvent        string         `yaml:"bus_event"`
	Secret          string         `yaml:"secret"`
	SignatureHeader string         `yaml:"signature_header"`
	Response        map[string]any `yaml:"response"`
}

type AIConfig struct {
	DefaultProvider string                    `yaml:"default_provider"`
	DefaultModel    string                    `yaml:"default_model"`
	Providers       map[string]ProviderConfig `yaml:"providers"`
}

type ProviderConfig struct {
	APIKey    string `yaml:"api_key"`
	BaseURL   string `yaml:"base_url"`
	AuthToken string `yaml:"auth_token"`
}

type AgentModelConfig struct {
	Model    string `yaml:"model"`
	Provider string `yaml:"provider"`
}

type FallbackModelConfig struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

type AgentDefConfig struct {
	ID                   string                `yaml:"id"`
	Name                 string                `yaml:"name"`
	Description          string                `yaml:"description"`
	Tags                 []string              `yaml:"tags"`
	SystemPrompt         string                `yaml:"system_prompt"`
	Model                string                `yaml:"model"`
	Provider             string                `yaml:"provider"`
	Temperature          float32               `yaml:"temperature"`
	MaxTokens            int                   `yaml:"max_tokens"`
	Tools                []string              `yaml:"tools"`
	MCPs                 []string              `yaml:"mcps"`
	Memory               AgentMemoryConfig     `yaml:"memory"`
	MemoryModel          AgentModelConfig      `yaml:"memory_model"`
	SummaryModel         AgentModelConfig      `yaml:"summary_model"`
	FallbackModels       []FallbackModelConfig `yaml:"fallback_models"`
	CompactionThreshold  int                   `yaml:"compaction_threshold"`
	CompactionKeepRecent int                   `yaml:"compaction_keep_recent"`
	RestartPolicy        string                `yaml:"restart_policy"`
	MaxRestarts          int                   `yaml:"max_restarts"`
	HealthCheck          HealthCheckConfig     `yaml:"health_check"`
	Triggers             AgentTriggersConfig   `yaml:"triggers"`
	Env                  map[string]string     `yaml:"env"`
}

type AgentMemoryConfig struct {
	Enabled    bool   `yaml:"enabled"`
	Namespace  string `yaml:"namespace"`
	MaxEntries int    `yaml:"max_entries"`
	Summarize  bool   `yaml:"summarize"`
}

type HealthCheckConfig struct {
	IntervalSeconds int            `yaml:"interval_seconds"`
	ToolName        string         `yaml:"tool_name"`
	ToolInput       map[string]any `yaml:"tool_input"`
	PingPrompt      string         `yaml:"ping_prompt"`
}

type AgentTriggersConfig struct {
	Webhooks   []string                `yaml:"webhooks"`
	Schedules  []ScheduleTriggerConfig `yaml:"schedules"`
	Events     []string                `yaml:"events"`
	RunOnStart bool                    `yaml:"run_on_start"`
}

type ScheduleTriggerConfig struct {
	Cron    string         `yaml:"cron"`
	Payload map[string]any `yaml:"payload"`
}
