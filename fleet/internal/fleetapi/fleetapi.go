// Package fleetapi is the wire contract between fleetd and its rich clients,
// fleetctl's TUI and ssh: the watch snapshot, screen previews, terminal
// frames and the access listing. fleetd fills these types and the TUI reads
// them and nothing else, so either side can be built against the other's
// fake (see Fake). docs/superpowers/specs/2026-10-25-fleetctl-tui-access-design.md.
package fleetapi

import (
	"encoding/json"
	"time"
)

// Routes.
const (
	PathWatch   = "/v1/watch"   // GET ?since=<seq>: long poll, a Snapshot
	PathScreens = "/v1/screens" // GET ?workers=a,b&lines=N: a ScreensReply
	PathTerm    = "/v1/term/"   // GET /v1/term/{worker}?mode=&cols=&rows=: websocket
	PathAccess  = "/v1/access"  // GET: an AccessList; POST an AccessChange: the new AccessList
)

// Role is what a caller may do: an admin everything, a guest only the
// sandbox agents lent to its device.
type Role string

const (
	RoleAdmin Role = "admin"
	RoleGuest Role = "guest"
)

// Snapshot is the whole fleet as one caller may see it. A guest's holds only
// its lent agents, its own tasks and their events.
type Snapshot struct {
	Seq     uint64    `json:"seq"` // grows with every new snapshot; the next watch's since
	At      time.Time `json:"at"`
	Me      Me        `json:"me"`
	Workers []Worker  `json:"workers"` // in roster order: agent, then slot
	Tasks   []Task    `json:"tasks"`   // open, and closed in the last 24 h; newest first
	Events  []Event   `json:"events"`  // the last 200, newest first
	Quota   []Account `json:"quota"`
	Today   Today     `json:"today"`
	Hosts   []Host    `json:"hosts"` // a guest's: only its lent containers
	Fleetd  Fleetd    `json:"fleetd"`
	Knocks  int       `json:"knocks"` // devices that asked for access and are not tagged; admins only
}

// Me is the caller, as fleetd sees it.
type Me struct {
	Name   string   `json:"name"` // the login; "operator" for the full token
	Role   Role     `json:"role"`
	Device string   `json:"device,omitempty"` // the tailnet name; empty for the full token
	Lent   []string `json:"lent,omitempty"`   // a guest's agents
}

// Worker is one agent slot: daemon.Row's fields, with the same JSON names,
// plus what the TUI draws from.
type Worker struct {
	Agent       string    `json:"agent"` // agent-03, agent-03:2
	SlotOf      string    `json:"slot_of"`
	Slot        int       `json:"slot"`
	Host        string    `json:"host"`
	Container   string    `json:"container"` // joins Host.Containers
	Account     string    `json:"account"`
	State       string    `json:"state"` // standby | working | waiting | cooling | disabled | down | code_missing
	Task        string    `json:"task,omitempty"`
	Idle        string    `json:"idle"`
	IdleS       int64     `json:"idle_s"`
	Model       string    `json:"model,omitempty"`
	Alerts      []string  `json:"alerts"`
	RecordedAt  time.Time `json:"recorded_at,omitzero"`
	FiveHour    float64   `json:"five_hour_pct"`
	SevenDay    float64   `json:"seven_day_pct"`
	FiveResets  time.Time `json:"five_hour_resets,omitzero"`
	SevenResets time.Time `json:"seven_day_resets,omitzero"`
	QuotaAt     time.Time `json:"quota_as_of,omitzero"`
	Context     float64   `json:"context_pct"`
	Low         bool      `json:"low,omitempty"`
	Reserved    bool      `json:"reserved,omitempty"`
	Models      []string  `json:"models"`
	Session     string    `json:"session,omitempty"`
	Reason      string    `json:"disabled_reason,omitempty"`
	Cooling     time.Time `json:"cooling_until,omitzero"`
	Unreachable bool      `json:"unreachable,omitempty"`
	Rotating    bool      `json:"rotating,omitempty"`

	Sandbox  bool     `json:"sandbox,omitempty"`
	Activity Activity `json:"activity"`
}

// Activity is what a worker is doing now, from its telemetry and phase: what
// its sprite acts out. fleetd sets it; a sprite never moves without it.
type Activity struct {
	Kind  ActivityKind `json:"kind"`
	Tool  string       `json:"tool,omitempty"` // the last tool, e.g. Bash
	Since time.Time    `json:"since,omitzero"`
}

type ActivityKind string

const (
	ActThinking ActivityKind = "thinking" // working, no tool event in 20 s
	ActReading  ActivityKind = "reading"  // Read, Grep, Glob, WebFetch, WebSearch
	ActEditing  ActivityKind = "editing"  // Edit, Write, NotebookEdit
	ActRunning  ActivityKind = "running"  // Bash, and any other tool
	ActPushing  ActivityKind = "pushing"  // Bash running git push
	ActTesting  ActivityKind = "testing"  // Bash running a test command
	ActWaiting  ActivityKind = "waiting"  // state waiting: on a person
	ActIdle     ActivityKind = "idle"     // standby
	ActCooling  ActivityKind = "cooling"
	ActDown     ActivityKind = "down" // down, disabled, code_missing, unreachable
)

// Task is ledger.Task's fields, with the same JSON names, plus its thread's
// size and latest message for the list.
type Task struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`
	Assigner  string    `json:"assigner"`
	Outcome   string    `json:"outcome,omitempty"`
	Repo      string    `json:"repo,omitempty"`
	Model     string    `json:"model,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	PR        string    `json:"pr,omitempty"`
	Assigned  time.Time `json:"assigned,omitzero"`
	Closed    time.Time `json:"closed,omitzero"` // zero: open
	AdminWork bool      `json:"admin_work"`      // may use memory, the gateway and secrets

	Messages int       `json:"messages"`
	Last     string    `json:"last,omitempty"` // the latest message's first line, at most 200 bytes
	LastAt   time.Time `json:"last_at,omitzero"`
	LastKind string    `json:"last_kind,omitempty"` // report | question | progress
}

// Event is one ledger event. Detail is the event's JSON object.
type Event struct {
	ID      int64           `json:"id"`
	At      time.Time       `json:"at"`
	Agent   string          `json:"agent"`
	Kind    string          `json:"kind"`
	Session string          `json:"session,omitempty"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// Account is one Claude subscription's quota. Orchestrator marks the
// orchestrator's own.
type Account struct {
	Name         string    `json:"name"`
	Agents       []string  `json:"agents"` // the agents on it
	Orchestrator bool      `json:"orchestrator,omitempty"`
	FiveHour     float64   `json:"five_hour_pct"`
	SevenDay     float64   `json:"seven_day_pct"`
	FiveResets   time.Time `json:"five_hour_resets,omitzero"`
	SevenResets  time.Time `json:"seven_day_resets,omitzero"`
	At           time.Time `json:"at,omitzero"`
	// BurnPerHour is the 5-hour window's growth in percentage points per
	// hour, a least-squares fit of the last hour's samples.
	BurnPerHour float64 `json:"burn_per_hour"`
	// RunOut is when the 5-hour window reaches 100 % at that rate; zero
	// when it resets first.
	RunOut  time.Time `json:"run_out,omitzero"`
	History []Point   `json:"history"` // the 5-hour window's %, last 5 h
}

// Point is one sample of a series.
type Point struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

// Today is the day so far (since local midnight on fleetd's host).
type Today struct {
	Since        time.Time `json:"since"`
	TasksClosed  int       `json:"tasks_closed"`
	SpendUSD     float64   `json:"spend_usd"`
	OutputTokens int64     `json:"output_tokens"`
}

// Host is one machine running agents.
type Host struct {
	Name       string      `json:"name"`
	Reachable  bool        `json:"reachable"`
	CPUs       int         `json:"cpus"`
	Load       [3]float64  `json:"load"`
	MemUsed    int64       `json:"mem_used"`
	MemTotal   int64       `json:"mem_total"`
	DiskUsed   int64       `json:"disk_used"` // the code directory's filesystem
	DiskTotal  int64       `json:"disk_total"`
	At         time.Time   `json:"at,omitzero"`
	Load1      []Point     `json:"load1"` // last 60 samples
	Containers []Container `json:"containers"`
}

// Container is one agent container's resources.
type Container struct {
	Name     string    `json:"name"`
	Agent    string    `json:"agent"`
	Running  bool      `json:"running"`
	CPU      float64   `json:"cpu_pct"`
	Mem      int64     `json:"mem"`
	MemLimit int64     `json:"mem_limit"`
	Restarts int       `json:"restarts"` // in the last 24 h
	At       time.Time `json:"at,omitzero"`
	CPUHist  []Point   `json:"cpu_hist"` // last 60 samples
	MemHist  []Point   `json:"mem_hist"`
}

// Fleetd is fleetd itself.
type Fleetd struct {
	Version    string    `json:"version"`
	Started    time.Time `json:"started"`
	TickAt     time.Time `json:"tick_at,omitzero"`
	TickMS     int64     `json:"tick_ms"`
	TickErrors []string  `json:"tick_errors,omitempty"`
}

// ScreensReply answers GET /v1/screens.
type ScreensReply struct {
	Screens []Screen `json:"screens"`
}

// Screen is a worker's last lines, ANSI kept. Error says why there are none.
type Screen struct {
	Worker string    `json:"worker"`
	Text   string    `json:"text"`
	Cols   int       `json:"cols"`
	At     time.Time `json:"at"`
	Error  string    `json:"error,omitempty"`
}

// TermMode is how a terminal session joins a worker.
type TermMode string

const (
	TermWatch TermMode = "watch" // read-only; never resizes the session
	TermType  TermMode = "type"  // the keyboard: one typist per worker
	TermShell TermMode = "shell" // a login shell in the container
)

// TermFrame is a JSON text frame on /v1/term; terminal bytes are binary
// frames. The client sends resize; fleetd sends the rest.
type TermFrame struct {
	Type   string   `json:"type"` // see Frame*
	Worker string   `json:"worker,omitempty"`
	Mode   TermMode `json:"mode,omitempty"`
	Cols   int      `json:"cols,omitempty"`
	Rows   int      `json:"rows,omitempty"`
	Holder string   `json:"holder,omitempty"` // held: who has the keyboard
	Error  string   `json:"error,omitempty"`
}

const (
	FrameHello    = "hello"    // fleetd: the session is up (worker, mode, size)
	FrameResize   = "resize"   // client: the terminal's new size
	FrameHeld     = "held"     // fleetd: someone else types (holder); the session did not start
	FrameReleased = "released" // fleetd: an admin took the keyboard; you watch now
	FrameError    = "error"    // fleetd: the session ended on an error
	FrameExit     = "exit"     // fleetd: the session ended (the shell exited, tmux detached)
)

// Device is a tagged device. StableID is what a tag binds; Name is shown.
type Device struct {
	StableID string    `json:"stable_id"`
	Name     string    `json:"name"` // friend-laptop.tail1234.ts.net
	Login    string    `json:"login"`
	Role     Role      `json:"role"`
	Agents   []string  `json:"agents,omitempty"` // a guest's lent agents
	TaggedAt time.Time `json:"tagged_at"`
	TaggedBy string    `json:"tagged_by"`
	LastSeen time.Time `json:"last_seen,omitzero"`
}

// Knock is an untagged device that asked for access.
type Knock struct {
	StableID string    `json:"stable_id"`
	Name     string    `json:"name"`
	Login    string    `json:"login"`
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
	Count    int       `json:"count"`
}

// AccessList answers GET /v1/access.
type AccessList struct {
	Devices []Device `json:"devices"`
	Knocks  []Knock  `json:"knocks"` // newest first
}

// AccessChange is POST /v1/access. Device is a full tailnet name, a short
// name that matches exactly one known device, or a StableID.
type AccessChange struct {
	Op     string   `json:"op"` // see Access*
	Device string   `json:"device"`
	Agents []string `json:"agents,omitempty"` // lend, unlend
}

const (
	AccessAdmin  = "admin"
	AccessLend   = "lend"
	AccessUnlend = "unlend"
	AccessRevoke = "revoke"
)
