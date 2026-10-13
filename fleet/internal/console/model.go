// Package console is the fleet console: it observes the agents on its own
// host live, merges fleetd's records, and serves the result as one snapshot
// and its patches. model.go is the contract the panel (console/src/model.ts)
// mirrors field for field.
package console

import (
	"encoding/json"
	"time"
)

// Source keys.
const (
	SrcTerminals  = "terminals"
	SrcEvents     = "events"
	SrcStatusline = "statusline"
	SrcDocker     = "docker"
	SrcHost       = "host"
	SrcGit        = "git"
	SrcFleetd     = "fleetd"
)

type Source struct {
	OK    bool      `json:"ok"`
	At    time.Time `json:"at,omitzero"`
	Error string    `json:"error,omitempty"`
}

type Snapshot struct {
	At           time.Time           `json:"at"`
	Build        string              `json:"build"` // see BuildID: the panel reloads when it changes
	Sources      map[string]Source   `json:"sources"`
	Host         HostInfo            `json:"host"`
	Orchestrator *Orchestrator       `json:"orchestrator"`
	Tasks        []Task              `json:"tasks"`
	Agents       []Agent             `json:"agents"`
	Events       []Event             `json:"events"`
	Today        Today               `json:"today"`
	Git          Git                 `json:"git"`
	Activity     map[string][]Bucket `json:"activity"`
}

type HostInfo struct {
	Name     string     `json:"name"`
	UptimeS  int64      `json:"uptime_s"`
	Load     [3]float64 `json:"load"`
	MemUsed  int64      `json:"mem_used"`
	MemTotal int64      `json:"mem_total"`
	CPUs     int        `json:"cpus"`
	At       time.Time  `json:"at"`
}

type Orchestrator struct {
	Name           string    `json:"name"`
	FiveHourPct    float64   `json:"five_hour_pct"`
	SevenDayPct    float64   `json:"seven_day_pct"`
	FiveHourResets time.Time `json:"five_hour_resets,omitzero"`
	SevenDayResets time.Time `json:"seven_day_resets,omitzero"`
	At             time.Time `json:"at"`
}

type Task struct {
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	Assigner string    `json:"assigner"`
	Since    time.Time `json:"since"`
}

// Phases, as fleetd's reconciler names them.
const (
	PhaseStandby  = "standby"
	PhaseWorking  = "working"
	PhaseWaiting  = "waiting"
	PhaseCooling  = "cooling"
	PhaseDisabled = "disabled"
	PhaseDown     = "down"
	// PhaseCodeMissing: the container's code mount was empty; it restarts.
	PhaseCodeMissing = "code_missing"
)

type Agent struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Live bool   `json:"live"` // on the console's host: terminals and live sources exist

	// From fleetd (recorded, ~30 s).
	Phase      string    `json:"phase"`
	Alerts     []string  `json:"alerts"`
	Task       string    `json:"task,omitempty"`
	IdleS      int64     `json:"idle_s"`
	Models     []string  `json:"models"`
	Session    string    `json:"session,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`

	// From the status line (live, 5 s).
	Model          string    `json:"model,omitempty"`
	ContextPct     float64   `json:"context_pct"`
	FiveHourPct    float64   `json:"five_hour_pct"`
	SevenDayPct    float64   `json:"seven_day_pct"`
	FiveHourResets time.Time `json:"five_hour_resets,omitzero"`
	StatuslineAt   time.Time `json:"statusline_at,omitzero"`

	// From live hook events.
	CurrentTool *Tool     `json:"current_tool"`
	LastEventAt time.Time `json:"last_event_at,omitzero"`

	Container *Container `json:"container"`
	LastTask  *LastTask  `json:"last_task"`
	Term      TermSize   `json:"term"`
}

type Tool struct {
	Name  string    `json:"name"`
	Since time.Time `json:"since"`
}

type Container struct {
	Running  bool      `json:"running"`
	CPUPct   float64   `json:"cpu_pct"`
	Mem      int64     `json:"mem"`
	MemLimit int64     `json:"mem_limit"`
	NetRx    int64     `json:"net_rx"`
	NetTx    int64     `json:"net_tx"`
	At       time.Time `json:"at"`
}

type LastTask struct {
	ID      string    `json:"id"`
	Outcome string    `json:"outcome"`
	Ended   time.Time `json:"ended"`
	CostUSD float64   `json:"cost_usd"`
}

type TermSize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Event is one stream entry. ID is fleetd's; 0 while only the live tail has it.
type Event struct {
	ID      int64           `json:"id"`
	At      time.Time       `json:"at"`
	Agent   string          `json:"agent"`
	Kind    string          `json:"kind"`
	Session string          `json:"session,omitempty"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// StreamKinds are the events a person reads; tool-level events drive
// current_tool and activity only. A kind matches when equal, or when the
// entry ends in "." and the kind has it as a prefix.
var StreamKinds = []string{"fleet.", "SessionStart", "SessionEnd", "UserPromptSubmit", "Stop", "StopFailure", "Notification"}

type Today struct {
	Since        time.Time `json:"since"`
	TasksClosed  int       `json:"tasks_closed"`
	SpendUSD     float64   `json:"spend_usd"`
	OutputTokens int64     `json:"output_tokens"`
}

type Git struct {
	Repos []Repo `json:"repos"`
}

type Repo struct {
	Name      string    `json:"name"`
	Main      Commit    `json:"main"`
	Branches  []Branch  `json:"branches"`
	Commits7d int       `json:"commits_7d"`
	At        time.Time `json:"at"`
}

type Commit struct {
	SHA     string    `json:"sha"`
	Subject string    `json:"subject"`
	At      time.Time `json:"at"`
}

type Branch struct {
	Name    string    `json:"name"`
	Agent   string    `json:"agent"`
	SHA     string    `json:"sha"`
	Subject string    `json:"subject"`
	At      time.Time `json:"at"`
}

type Bucket struct {
	T time.Time `json:"t"`
	N int       `json:"n"`
}

// Message is one frame of /v1/watch.
type Message struct {
	Type     string    `json:"type"` // "snapshot" | "patch"
	Seq      uint64    `json:"seq"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
	Ops      []PatchOp `json:"ops,omitempty"`
}

// PatchOp is one RFC 6902 operation (add, remove, replace).
type PatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

// TermFrame is a JSON text frame on /v1/term; terminal bytes are binary frames.
type TermFrame struct {
	Type   string `json:"type"` // "hello" | "error" | "reconnecting" | "released"
	Agent  string `json:"agent,omitempty"`
	Mode   string `json:"mode,omitempty"` // "watch" | "control"
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Holder string `json:"holder,omitempty"`
	Error  string `json:"error,omitempty"`
}
