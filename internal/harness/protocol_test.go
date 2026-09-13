package harness

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
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

// TodoWrite is where Claude Code keeps a plan, so that is where we read one.
func TestPlanFromTodoWrite(t *testing.T) {
	in := json.RawMessage(`{"todos":[
		{"content":"Read the spec","status":"completed","activeForm":"Reading the spec"},
		{"content":"Write the test","status":"in_progress","activeForm":"Writing the test"}
	]}`)
	got := planFrom(in)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Content != "Read the spec" || got[0].Status != "completed" {
		t.Errorf("first entry wrong: %+v", got[0])
	}
	if got[1].ActiveForm != "Writing the test" {
		t.Errorf("activeForm lost: %+v", got[1])
	}
}

// An older or newer TodoWrite shape must not cost us the whole plan.
func TestPlanFromToleratesAMissingPriority(t *testing.T) {
	got := planFrom(json.RawMessage(`{"todos":[{"content":"x","status":"pending"}]}`))
	if len(got) != 1 || got[0].Priority != "" {
		t.Errorf("got %+v", got)
	}
}

func TestPlanFromRejectsRubbish(t *testing.T) {
	if got := planFrom(json.RawMessage(`{"todos":"not a list"}`)); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
	if got := planFrom(nil); got != nil {
		t.Errorf("nil input: got %+v, want nil", got)
	}
}

// A tool_result's content is written either as a plain string or as a list of
// blocks. Reading only one shape is how a transcript loses half its output —
// the same trap internal/chatlog fell into with message content.
func TestToolResultTextReadsBothShapes(t *testing.T) {
	if got := toolResultText(json.RawMessage(`"total 4\n"`)); got != "total 4\n" {
		t.Errorf("string shape: got %q", got)
	}
	blocks := json.RawMessage(`[{"type":"text","text":"one "},{"type":"text","text":"two"}]`)
	if got := toolResultText(blocks); got != "one two" {
		t.Errorf("block shape: got %q", got)
	}
	if got := toolResultText(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	if got := toolResultText(json.RawMessage(`{"unexpected":true}`)); got != "" {
		t.Errorf("object: got %q", got)
	}
}

func TestTruncateOutputLeavesShortResultsAlone(t *testing.T) {
	if got := truncateOutput("hello"); got != "hello" {
		t.Errorf("got %q", got)
	}
}

// A tool result can be a whole file. The transcript shows a preview; the bytes
// are of no use to it and paying to stream them is worse than useless.
func TestTruncateOutputCapsAndMarks(t *testing.T) {
	got := truncateOutput(strings.Repeat("x", 5000))
	if len(got) > maxToolOutput+len("\n…") {
		t.Errorf("got %d bytes, want at most %d", len(got), maxToolOutput+len("\n…"))
	}
	if !strings.HasSuffix(got, "\n…") {
		t.Errorf("no truncation marker: %q", got[len(got)-10:])
	}
}

// Cutting mid-rune puts U+FFFD on screen.
func TestTruncateOutputCutsOnARuneBoundary(t *testing.T) {
	got := truncateOutput(strings.Repeat("é", 4000))
	if !utf8.ValidString(strings.TrimSuffix(got, "\n…")) {
		t.Error("truncation produced invalid UTF-8")
	}
}
