package harness

import (
	"encoding/json"
	"testing"
)

// The real event stream, in the order the CLI emits it. Recorded from a live
// run that ran a shell command, because a parser written against a guessed
// shape is a parser that fails the first time it meets the real thing.
const recorded = `
{"type":"rate_limit_event","session_id":"s1","rate_limit_info":{"status":"allowed_warning","rateLimitType":"seven_day","utilization":0.88,"resetsAt":1788019200,"unifiedWindows":{"five_hour":{"utilization":0.12,"resetsAt":1787981400},"seven_day":{"utilization":0.88,"resetsAt":1788019200}}}}
{"type":"system","subtype":"init","session_id":"s1","cwd":"/tmp"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"karmax tool call app.push title=Hi"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_01","is_error":false}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"DONE"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"DONE","total_cost_usd":0.0683,"num_turns":1,"duration_ms":1815,"usage":{"input_tokens":2,"output_tokens":4,"cache_read_input_tokens":12771,"cache_creation_input_tokens":6184}}
`

func TestParsesARealTurn(t *testing.T) {
	var turn Turn
	for _, line := range splitLines(recorded) {
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line did not decode: %v\n%s", err, line)
		}
		switch ev.Type {
		case "rate_limit_event":
			turn.Limits = ev.RateLimitInfo
		case "assistant":
			for _, c := range ev.Message.Content {
				if c.Type == "tool_use" {
					turn.ToolCalls = append(turn.ToolCalls, ToolCall{
						Name: c.Name, Input: c.Input, Command: shellCommand(c.Name, c.Input)})
				}
			}
		case "result":
			turn.Text, turn.Usage, turn.CostUSD = ev.Result, ev.Usage, ev.TotalCostUSD
		}
	}

	if turn.Text != "DONE" {
		t.Errorf("text = %q", turn.Text)
	}
	if turn.CostUSD == 0 {
		t.Error("cost was not captured; the budget ledger would record nothing")
	}
	if turn.Usage.CacheReadTokens != 12771 {
		t.Errorf("cache reads = %d, want 12771 — this is the per-turn overhead the design turns on", turn.Usage.CacheReadTokens)
	}
	if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].Command != "karmax" {
		t.Errorf("tool calls = %+v, want one Bash running karmax", turn.ToolCalls)
	}
	if turn.Limits == nil {
		t.Fatal("rate limit info was dropped; the breaker would be blind")
	}
	name, worst := turn.Limits.Worst()
	if name != "seven_day" || worst.Utilization != 0.88 {
		t.Errorf("worst window = %s at %.2f, want seven_day at 0.88", name, worst.Utilization)
	}
}

// An unknown event type must not break a turn: the CLI adds them over time and
// a parser that dies on one is a parser that dies on an upgrade.
func TestUnknownEventsAreIgnored(t *testing.T) {
	var ev event
	if err := json.Unmarshal([]byte(`{"type":"something_new","payload":{"a":1}}`), &ev); err != nil {
		t.Fatalf("an unknown event should decode harmlessly: %v", err)
	}
	if ev.Type != "something_new" {
		t.Errorf("type = %q", ev.Type)
	}
}

func TestShellCommandExtraction(t *testing.T) {
	cases := map[string]string{
		`{"command":"karmax tool call comms.send to=x"}`: "karmax",
		`{"command":"/usr/bin/git status"}`:              "git",
		`{"command":"FOO=bar wacli send --to y"}`:        "wacli",
		`{"command":""}`: "",
	}
	for in, want := range cases {
		if got := shellCommand("Bash", json.RawMessage(in)); got != want {
			t.Errorf("shellCommand(%s) = %q, want %q", in, got, want)
		}
	}
	// A non-shell tool has no command to audit.
	if got := shellCommand("Read", json.RawMessage(`{"file_path":"/etc/passwd"}`)); got != "" {
		t.Errorf("non-shell tool returned %q", got)
	}
}

func TestUserEventShape(t *testing.T) {
	b, err := userEvent("hello")
	if err != nil {
		t.Fatal(err)
	}
	if b[len(b)-1] != '\n' {
		t.Error("the event must be newline-terminated or the CLI never sees it")
	}
	var m map[string]any
	if err := json.Unmarshal(b[:len(b)-1], &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "user" {
		t.Errorf("type = %v", m["type"])
	}
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			if len(cur) > 2 {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if len(cur) > 2 {
		out = append(out, cur)
	}
	return out
}
