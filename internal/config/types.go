package config

import (
	"github.com/MelloB1989/karmax/internal/mcp"
)

type KarmaxConfig struct {
	Karmax    KarmaxCoreConfig      `yaml:"karmax"`
	Dashboard DashboardConfig       `yaml:"dashboard"`
	Webhooks  WebhooksConfig        `yaml:"webhooks"`
	AI        AIConfig              `yaml:"ai"`
	MCPs      []mcp.MCPServerConfig `yaml:"mcps"`
	Comms     CommsConfig           `yaml:"comms"`
	Agents    []AgentDefConfig      `yaml:"agents"`
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
	Version  string `yaml:"version"`
	DataDir  string `yaml:"data_dir"`
	LogLevel string `yaml:"log_level"`
	LogFormat string `yaml:"log_format"`
}

type DashboardConfig struct {
	Enabled bool   `yaml:"enabled"`
	Port    int    `yaml:"port"`
	Host    string `yaml:"host"`
}

type WebhooksConfig struct {
	Enabled bool              `yaml:"enabled"`
	Port    int               `yaml:"port"`
	Host    string            `yaml:"host"`
	Routes  []WebhookRouteConfig `yaml:"routes"`
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
	DefaultProvider string                   `yaml:"default_provider"`
	DefaultModel    string                   `yaml:"default_model"`
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
