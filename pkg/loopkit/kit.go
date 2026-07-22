package loopkit

import (
	"context"
	"encoding/json"
	"time"
)

// ChatSummaryRecord is the durable per-chat "cold memory" summary that powers
// the memory retrieval sub-agent. The cold-scan loop is the main writer.
type ChatSummaryRecord struct {
	ChatJID         string
	ChatName        string
	IsGroup         bool
	Summary         string
	MessageCount    int
	OwnMessageCount int
	LastMessageAt   time.Time
	SummarizedAt    time.Time
	Status          string // pending | summarized | hot | skipped
}

// Kit is the capability surface a loop receives when it runs. KARMAX implements
// it and passes it to Loop.Run. Loop authors depend ONLY on this interface, not
// on KARMAX's internal packages — so third-party loops stay decoupled and
// compile against just this SDK.
type Kit interface {
	// Ask runs a prompt through the operator's main agent — full toolset,
	// long-term memory, and judgement — and returns its reply. Use for tasks
	// that need reasoning or the agent's tools (sending messages, scheduling,
	// etc.). Consumes the agent's model budget, so prefer Harness for pure
	// web/text work.
	Ask(ctx context.Context, prompt string) (string, error)

	// Harness runs a prompt directly through the Claude Code CLI (web search,
	// file, and shell tools) and returns its text output. It runs on the Claude
	// subscription, independent of the main model — ideal for web research and
	// heavy work even when the main model is rate-limited.
	Harness(ctx context.Context, prompt string) (string, error)

	// Remember stores a durable, standalone fact in the operator's long-term
	// memory (tagged with the loop name).
	Remember(fact string) error

	// Recall returns up to limit memory snippets semantically matching query.
	Recall(query string, limit int) ([]string, error)

	// Notify sends a notification to the operator's phone app: it is saved to
	// the in-app feed AND delivered as a push (so it survives a missed push).
	// Notifications are informational only — for anything that needs the
	// operator's DECISION, use Propose instead.
	Notify(title, body string) error

	// Propose creates a pending approval in the operator's approvals inbox
	// (with a push). The operator can approve — which hands `action` to the
	// agent to EXECUTE — or reject it. Use this, never Notify, when a loop
	// surfaces something requiring a decision (a suggested reply, a
	// commitment, money, anything sensitive). `action` must be concrete and
	// self-contained (include the draft text and the target), since it is
	// executed as written on approval.
	Propose(title, summary, action string) error

	// Remind creates a reminder on the operator's phone (additive, no
	// approval). Use for things ONLY the operator can personally do — send a
	// document the assistant doesn't have, reply in a personal chat, an
	// offline errand. due is an optional ISO-8601 datetime with timezone;
	// notes is optional context shown with the reminder.
	Remind(title, due, notes string) error

	// SendWhatsApp sends a message through the operator's connected WhatsApp
	// account to target (a phone number, contact name, or chat JID). Use for
	// loops that deliver to a specific recipient rather than the app.
	SendWhatsApp(ctx context.Context, target, content string) error

	// ReadWhatsApp returns recent WhatsApp messages as formatted text. Pass a
	// chat name or JID, or "" for the most recent messages across chats.
	ReadWhatsApp(ctx context.Context, chat string, limit int) (string, error)

	// HTTP performs an HTTP request and returns the response body, status code,
	// and any transport error. Headers and body may be empty.
	HTTP(ctx context.Context, method, url string, headers map[string]string, body string) (string, int, error)

	// Trigger reports what fired this run (schedule / webhook / manual) and any
	// payload — e.g. for a webhook, Payload["body"] holds the request body.
	Trigger() Trigger

	// Config returns an install-time configuration value for this loop (for
	// example an API key the operator entered when installing it), or "" if the
	// key is unset.
	Config(key string) string

	// Logf writes a line to KARMAX's logs, prefixed with the loop's name.
	Logf(format string, args ...any)

	// RunLoop triggers another registered loop by name (manual trigger). Lets a
	// loop hand work to a dedicated loop rather than doing it inline.
	RunLoop(name string) error

	// HostTool resolves a host-side dependency KARMAX knows about: "wacli" and
	// "gws" return binary paths (env override → PATH → well-known locations),
	// "karmax" the KARMAX CLI itself, and "wacli-api" the local wacli HTTP API
	// base URL. Returns the bare name when it cannot resolve further.
	HostTool(name string) string

	// Summarize runs a prompt through the agent's cheap SUMMARY model (not the
	// main model) — use it for bulk distillation that shouldn't burn the main
	// model's budget. No tools, no memory: prompt in, text out.
	Summarize(ctx context.Context, prompt string) (string, error)

	// Gateway runs a prompt against the agent's MAIN model through the karma
	// gateway: cheap and fast (no agent loop, no Claude Code run). This is the
	// path a loop should try FIRST.
	//
	// A loop may pass TEMPORARY TOOLS that exist only for this call — that is
	// how a loop adds capability without bloating KARMAX's core toolset. The
	// wa-monitor loop, for instance, lends the gateway a single `wacli` tool so
	// it can look up another chat mid-conversation. Tools are scoped to this
	// call: they vanish when it returns and are invisible to the main agent.
	//
	// Escalate to Harness only when the work needs the shell, files, or research
	// no lent tool can cover.
	Gateway(ctx context.Context, prompt string, tools ...Tool) (string, error)

	// ChatSummary returns the stored cold-memory summary for a chat JID (nil if
	// none); SaveChatSummary creates/updates one. These records feed the memory
	// retrieval sub-agent's per-chat context.
	ChatSummary(jid string) (*ChatSummaryRecord, error)
	SaveChatSummary(rec ChatSummaryRecord) error

	// --- Short-term memory (scratch KV) --------------------------------------
	//
	// Durable but EXPIRING key/value state, partitioned into groups the loop
	// names itself (wa-monitor uses one group per chat). Use it for the working
	// context long-term memory shouldn't hold: what you just told someone, a
	// "stop replying to X" instruction, the topic in play. The engine owns TTL
	// and expiry — expired entries simply stop being returned.
	//
	// ShortSet stores a value; ttl <= 0 means it never expires.
	// ShortGet returns (value, found). ShortAll returns every live entry in the
	// group, freshest first — handy to render straight into a prompt.
	ShortSet(group, key, value string, ttl time.Duration) error
	ShortGet(group, key string) (string, bool, error)
	ShortAll(group string) ([]ShortMemory, error)
	ShortForget(group, key string) error
	ShortClear(group string) error
}

// Tool is a capability a loop lends to the model for the duration of one
// Gateway call. Loops own these, so KARMAX's core toolset stays small while
// individual loops can be arbitrarily capable.
//
// Schema is a JSON-Schema object describing Run's arguments (same shape the
// model sees). Run receives the decoded arguments and returns the text handed
// back to the model; returning an error surfaces it to the model as a failure
// so it can adapt rather than crashing the run.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Run         func(ctx context.Context, args map[string]any) (string, error)
}

// ShortMemory is one short-term KV entry.
type ShortMemory struct {
	Key       string     `json:"key"`
	Value     string     `json:"value"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}
