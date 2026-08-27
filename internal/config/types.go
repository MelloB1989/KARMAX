package config

import (
	"github.com/MelloB1989/karmax/internal/mcp"
)

type KarmaxConfig struct {
	Karmax   KarmaxCoreConfig      `yaml:"karmax"`
	Database DatabaseConfig        `yaml:"database"`
	Webhooks WebhooksConfig        `yaml:"webhooks"`
	API      APIConfig             `yaml:"api"`
	Console  ConsoleConfig         `yaml:"console"`
	AI       AIConfig              `yaml:"ai"`
	MCPs     []mcp.MCPServerConfig `yaml:"mcps"`
	Comms    CommsConfig           `yaml:"comms"`
	Agents   []AgentDefConfig      `yaml:"agents"`
	Loops    []LoopConfig          `yaml:"loops"`
	ColdScan ColdScanConfig        `yaml:"cold_scan"`
}

// DatabaseConfig points the store at a backend. See store.ParseDSN for the
// accepted forms (postgres://, mysql://, a sqlite path).
type DatabaseConfig struct {
	// URL is the connection target. Empty keeps the SQLite file under
	// data_dir, which is what every existing install has.
	URL string `yaml:"url"`
}

// ColdScanConfig is DEPRECATED and ignored: cold-scan is now a marketplace
// loop (karmax loops install cold-scan), configured via KARMAX_LOOP_COLD_SCAN_*
// env keys. The struct remains only so older karmax.yaml files still parse.
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
	// Harness, when set (e.g. "claude_code"), runs the loop prompt DIRECTLY
	// through that coding harness and ingests its output to memory, bypassing
	// the main model. Use for web-research loops that must keep working even
	// when the main model is rate-limited.
	Harness string `yaml:"harness"`
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
	// BudgetUSDPerMonth is what the operator is willing to spend on inference.
	// Nothing enforces it — it is the line the cost view and the agent measure
	// the run rate against, so "we are over" is a fact either can state rather
	// than a number only the operator remembers. Zero means no budget set.
	BudgetUSDPerMonth float64 `yaml:"budget_usd_per_month"`
	// OAuthCallbackPort is the loopback port the browser is redirected back to
	// when connecting an OAuth integration. Fixed rather than ephemeral because
	// providers match redirect URLs exactly against what you registered, so the
	// port has to be one you can register. Zero uses the default (9095).
	OAuthCallbackPort int `yaml:"oauth_callback_port"`
	// OAuthCallbackHost is the host in that URL. Defaults to 127.0.0.1; set it
	// to "localhost" for a provider that refuses the bare-IP form.
	OAuthCallbackHost string `yaml:"oauth_callback_host"`
}

type WebhooksConfig struct {
	Enabled bool   `yaml:"enabled"`
	Port    int    `yaml:"port"`
	Host    string `yaml:"host"`

	// PublicURL is the address a third party can reach this webhook server at,
	// used to render the callback URL an operator pastes into GitHub or Jira.
	//
	// Separate from console.public_url because they are usually different
	// addresses: the console is a browser destination and the webhook server is
	// a machine destination, and on this deployment they are not even the same
	// port. Falls back to console.public_url when unset, which is right when a
	// single proxy fronts both.
	PublicURL string               `yaml:"public_url"`
	Routes    []WebhookRouteConfig `yaml:"routes"`
}

// APIConfig configures the HTTP API the phone app talks to. It binds to
// 0.0.0.0 so it is reachable over both the LAN and Tailscale. Auth is via the
// KARMAX_API_TOKEN environment variable.
type APIConfig struct {
	Enabled bool   `yaml:"enabled"`
	Port    int    `yaml:"port"`
	Host    string `yaml:"host"`
}

// ConsoleConfig is the web console's own listener.
//
// Deliberately a SEPARATE server from APIConfig rather than more routes on the
// same mux. The API port carries POST /api/tools/{name}, which can invoke
// shell.exec — that is remote code execution, and it must stay reachable only
// from an operator's network no matter how the console is exposed. Splitting
// the listener is what lets the console be published without publishing that.
type ConsoleConfig struct {
	Enabled bool   `yaml:"enabled"`
	Port    int    `yaml:"port"`
	Host    string `yaml:"host"`

	// PublicURL is the address operators actually reach the console at, used
	// to render connector callback URLs. A setup wizard that prints a callback
	// URL nobody can reach is worse than no wizard, and the server cannot infer
	// its own public name from behind a CDN.
	PublicURL string `yaml:"public_url"`

	// SessionHours is how long a login lasts. 0 means the default (12h).
	SessionHours int `yaml:"session_hours"`
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

	// Deployments maps a model name to the name it is deployed under, for
	// providers that address a DEPLOYMENT where OpenAI addresses a model —
	// Azure OpenAI being the one that matters here. Whoever created the
	// resource chose those names, so they cannot be known at compile time.
	//
	//	deployments:
	//	  gpt-5-mini: my-gpt-5-mini
	//
	// Omit it when the deployment is named after the model, which is the
	// common case and the default.
	Deployments map[string]string `yaml:"deployments"`
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
	CoreTools            []string              `yaml:"core_tools"`
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
