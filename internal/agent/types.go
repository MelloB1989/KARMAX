package agent

type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "always"
	RestartOnFailure RestartPolicy = "on-failure"
	RestartNever     RestartPolicy = "never"
)

type AgentStatus string

const (
	StatusIdle     AgentStatus = "idle"
	StatusRunning  AgentStatus = "running"
	StatusPaused   AgentStatus = "paused"
	StatusStopping AgentStatus = "stopping"
	StatusStopped  AgentStatus = "stopped"
	StatusFailed   AgentStatus = "failed"
	StatusCrashed  AgentStatus = "crashed"
)

// ModelConfig specifies which model and provider to use for a sub-model
// (memory retrieval, summary/compaction, etc.).
type ModelConfig struct {
	Model    string `yaml:"model" json:"model"`
	Provider string `yaml:"provider" json:"provider"`
}

type AgentDef struct {
	ID                   string            `yaml:"id"                    json:"id"`
	Name                 string            `yaml:"name"                  json:"name"`
	Description          string            `yaml:"description"           json:"description"`
	Tags                 []string          `yaml:"tags"                  json:"tags"`
	SystemPrompt         string            `yaml:"system_prompt"         json:"system_prompt"`
	Model                string            `yaml:"model"                 json:"model"`
	Provider             string            `yaml:"provider"              json:"provider"`
	Temperature          float32           `yaml:"temperature"           json:"temperature"`
	MaxTokens            int               `yaml:"max_tokens"            json:"max_tokens"`
	Tools                []string          `yaml:"tools"                 json:"tools"`
	MCPs                 []string          `yaml:"mcps"                  json:"mcps"`
	Memory               AgentMemoryConfig `yaml:"memory"                json:"memory"`
	MemoryModelCfg       ModelConfig       `yaml:"memory_model"          json:"memory_model"`
	SummaryModelCfg      ModelConfig       `yaml:"summary_model"         json:"summary_model"`
	CompactionThreshold  int               `yaml:"compaction_threshold"  json:"compaction_threshold"`
	CompactionKeepRecent int               `yaml:"compaction_keep_recent" json:"compaction_keep_recent"`
	RestartPolicy        RestartPolicy     `yaml:"restart_policy"        json:"restart_policy"`
	MaxRestarts          int               `yaml:"max_restarts"          json:"max_restarts"`
	HealthCheck          HealthCheckConfig `yaml:"health_check"          json:"health_check"`
	Triggers             AgentTriggers     `yaml:"triggers"              json:"triggers"`
	Env                  map[string]string `yaml:"env"                   json:"env"`
}

type AgentMemoryConfig struct {
	Enabled    bool   `yaml:"enabled"     json:"enabled"`
	Namespace  string `yaml:"namespace"   json:"namespace"`
	MaxEntries int    `yaml:"max_entries" json:"max_entries"`
	Summarize  bool   `yaml:"summarize"   json:"summarize"`
}

type AgentTriggers struct {
	Webhooks   []string          `yaml:"webhooks"     json:"webhooks"`
	Schedules  []ScheduleTrigger `yaml:"schedules"    json:"schedules"`
	Events     []string          `yaml:"events"       json:"events"`
	RunOnStart bool              `yaml:"run_on_start" json:"run_on_start"`
}

type ScheduleTrigger struct {
	Cron    string         `yaml:"cron"    json:"cron"`
	Payload map[string]any `yaml:"payload" json:"payload"`
}

type HealthCheckConfig struct {
	IntervalSeconds int            `yaml:"interval_seconds" json:"interval_seconds"`
	ToolName        string         `yaml:"tool_name"        json:"tool_name"`
	ToolInput       map[string]any `yaml:"tool_input"       json:"tool_input"`
	PingPrompt      string         `yaml:"ping_prompt"      json:"ping_prompt"`
}

type AgentSnapshot struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Status    AgentStatus `json:"status"`
	Restarts  int         `json:"restarts"`
	StartedAt string      `json:"started_at,omitempty"`
	LastEvent string      `json:"last_event,omitempty"`
	LastErr   string      `json:"last_err,omitempty"`
	Uptime    string      `json:"uptime,omitempty"`
	Def       AgentDef    `json:"def"`
}
