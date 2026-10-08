// Package observe reads what Claude Code says about itself: the session list
// (`claude agents --json`), the status-line snapshot, the hook event log and
// the transcript. Every reader is tolerant: these are young interfaces, and
// a field that moves must cost a value, never the whole observation.
package observe

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
)

// Agent is one row of `claude agents --json`.
type Agent struct {
	ID        string `json:"id"`
	PID       int    `json:"pid"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"` // interactive | background
	StartedAt int64  `json:"startedAt"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"` // interactive: busy | waiting | idle
	// WaitingFor says what a waiting session waits on, e.g. "dialog open".
	WaitingFor string `json:"waitingFor"`
	State      string `json:"state"` // background: blocked | …
}

// Interactive reports whether the row is a terminal session.
func (a Agent) Interactive() bool { return a.Kind == "interactive" }

// Activity is status for an interactive session, state for a background one.
func (a Agent) Activity() string {
	if a.Status != "" {
		return a.Status
	}
	return a.State
}

// Started is when the session started.
func (a Agent) Started() time.Time { return time.UnixMilli(a.StartedAt) }

// ParseAgents reads `claude agents --json`, skipping anything printed before
// the array.
func ParseAgents(b []byte) ([]Agent, error) {
	if i := bytes.IndexByte(b, '['); i > 0 {
		b = b[i:]
	}
	var out []Agent
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Status is the status line's stdin, as the fleet's status script saved it.
type Status struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Model     struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	ContextWindow struct {
		UsedPercentage *float64 `json:"used_percentage"`
	} `json:"context_window"`
	Cost struct {
		TotalCostUSD float64 `json:"total_cost_usd"`
	} `json:"cost"`
	RateLimits struct {
		FiveHour *rawWindow `json:"five_hour"`
		SevenDay *rawWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

type rawWindow struct {
	UsedPercentage *float64        `json:"used_percentage"`
	ResetsAt       json.RawMessage `json:"resets_at"`
}

// Window is one of the account's rate-limit windows.
type Window struct {
	Used   float64 // percent
	Resets time.Time
}

// ParseStatus reads a status-line snapshot.
func ParseStatus(b []byte) (*Status, error) {
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ContextPct is the session's context use, 0 when unknown.
func (s *Status) ContextPct() float64 {
	if s.ContextWindow.UsedPercentage == nil {
		return 0
	}
	return *s.ContextWindow.UsedPercentage
}

// FiveHour is the account's five-hour window, if the session has reported one.
func (s *Status) FiveHour() (Window, bool) { return s.RateLimits.FiveHour.window() }

// SevenDay is the account's seven-day window, if reported.
func (s *Status) SevenDay() (Window, bool) { return s.RateLimits.SevenDay.window() }

func (w *rawWindow) window() (Window, bool) {
	if w == nil || w.UsedPercentage == nil {
		return Window{}, false
	}
	return Window{Used: *w.UsedPercentage, Resets: parseTime(w.ResetsAt)}, true
}

// parseTime reads a timestamp as unix seconds, unix milliseconds or RFC 3339:
// resets_at's encoding is not documented, so all three are accepted.
func parseTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
		raw = json.RawMessage(s)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	if n > 1e12 {
		return time.UnixMilli(int64(n))
	}
	return time.Unix(int64(n), 0)
}

// Event is one line of the hook log the fleet's event script appends.
type Event struct {
	T       int64           `json:"t"`
	Event   string          `json:"event"`
	Session string          `json:"session"`
	Tool    string          `json:"tool,omitempty"`
	OK      *bool           `json:"ok,omitempty"`
	Raw     json.RawMessage `json:"raw,omitempty"`
}

// Time is when the hook fired.
func (e Event) Time() time.Time { return time.Unix(e.T, 0) }

// ErrorType is a StopFailure's error type: rate_limit, authentication_failed,
// billing_error, account_on_hold, … Empty when there is none.
func (e Event) ErrorType() string {
	if len(e.Raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(e.Raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"error_type", "errorType", "error", "reason", "stop_reason", "type"} {
		switch v := m[k].(type) {
		case string:
			if v != "" && v != e.Event {
				return v
			}
		case map[string]any:
			if t, ok := v["type"].(string); ok && t != "" {
				return t
			}
		}
	}
	return findKnown(m)
}

// KnownErrors are the StopFailure types the reconciler acts on.
var KnownErrors = []string{"rate_limit", "authentication_failed", "billing_error", "account_on_hold"}

func findKnown(v any) string {
	switch x := v.(type) {
	case string:
		for _, k := range KnownErrors {
			if strings.Contains(x, k) {
				return k
			}
		}
	case map[string]any:
		for _, e := range x {
			if f := findKnown(e); f != "" {
				return f
			}
		}
	case []any:
		for _, e := range x {
			if f := findKnown(e); f != "" {
				return f
			}
		}
	}
	return ""
}

// ParseEvents reads the hook log, skipping lines it cannot parse.
func ParseEvents(r io.Reader) ([]Event, error) {
	var out []Event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Event == "" {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Usage is tokens by kind.
type Usage struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// Add sums two usages.
func (u Usage) Add(o Usage) Usage {
	return Usage{u.Input + o.Input, u.Output + o.Output, u.CacheRead + o.CacheRead, u.CacheCreation + o.CacheCreation}
}

// Summary is what a transcript says about its session.
type Summary struct {
	SessionID     string    `json:"session_id"`
	Cwd           string    `json:"cwd"`
	Started       time.Time `json:"started"`
	Ended         time.Time `json:"ended"`
	Turns         int       `json:"turns"`
	PeerMessages  int       `json:"peer_messages"`
	Usage         Usage     `json:"usage"`
	CostUSD       float64   `json:"cost_usd"` // list-price estimate, from the CLI's cost-state
	Models        []string  `json:"models"`
	LastAssistant string    `json:"last_assistant"`
}

type line struct {
	Type         string  `json:"type"`
	SessionID    string  `json:"sessionId"`
	Cwd          string  `json:"cwd"`
	Timestamp    string  `json:"timestamp"`
	TurnOrigin   string  `json:"turnOrigin"`
	IsMeta       bool    `json:"isMeta"`
	TotalCostUSD float64 `json:"totalCostUSD"`
	Message      struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Summarize reads a transcript (.jsonl).
//
// An assistant message is written as one line per content block, each
// carrying the same message id and the same usage; usage is counted once per
// id, or every tool call would double the tokens.
func Summarize(r io.Reader) (Summary, error) {
	var s Summary
	seen := map[string]bool{}
	models := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256<<10), 64<<20)
	for sc.Scan() {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if s.SessionID == "" {
			s.SessionID = l.SessionID
		}
		if s.Cwd == "" {
			s.Cwd = l.Cwd
		}
		if ts, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
			if s.Started.IsZero() || ts.Before(s.Started) {
				s.Started = ts
			}
			if ts.After(s.Ended) {
				s.Ended = ts
			}
		}
		switch l.Type {
		case "cost-state":
			s.CostUSD = l.TotalCostUSD
		case "user":
			if l.IsMeta || !isPrompt(l.Message.Content) {
				continue
			}
			s.Turns++
			if l.TurnOrigin == "peer" {
				s.PeerMessages++
			}
		case "assistant":
			m := l.Message
			if m.Model != "" && !models[m.Model] {
				models[m.Model] = true
				s.Models = append(s.Models, m.Model)
			}
			if m.ID == "" || !seen[m.ID] {
				seen[m.ID] = true
				s.Usage = s.Usage.Add(Usage{m.Usage.Input, m.Usage.Output, m.Usage.CacheRead, m.Usage.CacheCreation})
			}
			var blocks []block
			if json.Unmarshal(m.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
						s.LastAssistant = b.Text
					}
				}
			}
		}
	}
	return s, sc.Err()
}

// isPrompt reports whether a user message is a prompt rather than a tool
// result travelling back.
func isPrompt(content json.RawMessage) bool {
	var str string
	if json.Unmarshal(content, &str) == nil {
		return strings.TrimSpace(str) != ""
	}
	var blocks []block
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "text" {
			return true
		}
	}
	return false
}

// ProjectKey is the directory name Claude Code keeps a cwd's transcripts
// under, in ~/.claude/projects: every character that is not a letter or a
// digit becomes a dash.
func ProjectKey(cwd string) string {
	b := []byte(cwd)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}
