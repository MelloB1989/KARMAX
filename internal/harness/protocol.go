// Package harness runs coding harnesses as long-lived conversations.
//
// A harness call is normally one process per prompt, which costs a cold start
// every time: measured at 4.1s bare and 15.4s once a tool call is involved. Fed
// instead through --input-format stream-json, one process answers many messages
// and the second and third replies land in 1.6s and 1.8s — against the 15-18s
// the metered API path takes for the same work.
//
// That is the reason this package exists, and it also fixes the unit of work.
// Every turn carries roughly 12.7k cache-read and 6.2k cache-creation tokens of
// the CLI's own overhead. Amortised across a warm session that is cheap; paid
// per message it is ruinous. So the thing being managed here is a SESSION, and
// the supervisor's real job is deciding which ones stay warm.
package harness

import (
	"encoding/json"
	"strings"
)

// event is one line of --output-format stream-json.
//
// Only the fields this package acts on are decoded. The CLI emits more, and
// decoding what we do not use would turn every upstream addition into a parse
// failure.
type event struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	SessionID string `json:"session_id"`

	// assistant / user
	Message struct {
		Content []contentBlock `json:"content"`
	} `json:"message"`

	// stream_event, only present with --include-partial-messages
	StreamEvent struct {
		Type  string `json:"type"` // content_block_delta | message_start | …
		Delta struct {
			Type string `json:"type"` // text_delta
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`

	// rate_limit_event
	RateLimitInfo *RateLimit `json:"rate_limit_info"`

	// result
	IsError       bool    `json:"is_error"`
	APIErrorState string  `json:"api_error_status"`
	Result        string  `json:"result"`
	TotalCostUSD  float64 `json:"total_cost_usd"`
	NumTurns      int     `json:"num_turns"`
	DurationMS    int64   `json:"duration_ms"`
	Usage         Usage   `json:"usage"`
}

type contentBlock struct {
	Type string `json:"type"` // text | tool_use | tool_result
	Text string `json:"text"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
}

// Usage is what one turn consumed.
type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

// RateLimit is the harness's own account of how much quota is left.
//
// This is the difference between a budget that guesses and one that knows. The
// alternative — inferring a share from our own spend ledger — cannot see the
// operator's interactive sessions on the same account, which are most of the
// consumption. The harness reports both windows on every turn.
type RateLimit struct {
	Status             string            `json:"status"` // allowed | allowed_warning | …
	RateLimitType      string            `json:"rateLimitType"`
	Utilization        float64           `json:"utilization"`
	ResetsAt           int64             `json:"resetsAt"`
	IsUsingOverage     bool              `json:"isUsingOverage"`
	SurpassedThreshold float64           `json:"surpassedThreshold"`
	UnifiedWindows     map[string]Window `json:"unifiedWindows"`
}

// Window is one rate-limit window: the five-hour or the seven-day.
type Window struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
}

// Worst returns the most-consumed window, which is the one that will stop us.
func (r *RateLimit) Worst() (name string, w Window) {
	if r == nil {
		return "", Window{}
	}
	for n, win := range r.UnifiedWindows {
		if win.Utilization > w.Utilization {
			name, w = n, win
		}
	}
	return name, w
}

// ToolCall is one tool the harness invoked during a turn.
type ToolCall struct {
	Name  string
	Input json.RawMessage
	// Command is the first token of a shell invocation, for the audit
	// allowlist. Empty for anything that is not a shell tool.
	Command string
}

// Turn is one complete exchange: everything between sending a user message and
// the result event that closes it.
type Turn struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
	CostUSD   float64
	Limits    *RateLimit
	NumTurns  int
	Err       error
}

// userEvent is the single line written to stdin to ask a question.
func userEvent(text string) ([]byte, error) {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// shellCommand extracts the leading command from a shell tool's input, which is
// what the audit allowlist is checked against. Anything unparseable returns
// empty and is treated as unrecognised rather than allowed.
func shellCommand(name string, input json.RawMessage) string {
	if !strings.EqualFold(name, "Bash") && !strings.EqualFold(name, "Shell") {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	fields := strings.Fields(in.Command)
	if len(fields) == 0 {
		return ""
	}
	// "FOO=bar cmd" and "/usr/bin/cmd" both reduce to the program being run.
	for _, f := range fields {
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			continue
		}
		if i := strings.LastIndexByte(f, '/'); i >= 0 {
			f = f[i+1:]
		}
		return f
	}
	return ""
}
