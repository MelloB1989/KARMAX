package harness

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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
	got := truncateOutput(strings.Repeat("x", maxToolOutput*2))
	if len(got) > maxToolOutput+len("\n…") {
		t.Errorf("got %d bytes, want at most %d", len(got), maxToolOutput+len("\n…"))
	}
	if !strings.HasSuffix(got, "\n…") {
		t.Errorf("no truncation marker: %q", got[len(got)-10:])
	}
}

// Cutting mid-rune puts U+FFFD on screen. "→" is 3 bytes, and the cap is not a
// multiple of 3, so naïve byte-slicing at the cap lands mid-rune — which
// proves the rune-boundary logic.
func TestTruncateOutputCutsOnARuneBoundary(t *testing.T) {
	repeats := maxToolOutput // 3 bytes each, comfortably over the cap
	got := truncateOutput(strings.Repeat("→", repeats))
	trimmed := strings.TrimSuffix(got, "\n…")
	if !utf8.ValidString(trimmed) {
		t.Error("truncation produced invalid UTF-8")
	}
	if len(got) >= len(strings.Repeat("→", repeats)) {
		t.Error("truncation did not cap the result")
	}
}

// A long string anywhere in a tool's input is cut, the same shape of cap
// truncateOutput applies to a result — a giant file written through Write's
// "content" field is exactly the case this protects the wire from.
func TestTruncateJSONStringsCutsLongStringsAtAnyDepth(t *testing.T) {
	long := strings.Repeat("a", maxInputRunes*2)
	raw := json.RawMessage(`{"command":"echo hi","nested":{"note":"` + long + `"},"list":["short","` + long + `"]}`)

	got := truncateJSONStrings(raw)
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("truncated input is not valid json: %v", err)
	}
	if v["command"] != "echo hi" {
		t.Errorf("a short top-level string must survive untouched: %+v", v["command"])
	}
	nested, ok := v["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested object lost: %+v", v)
	}
	assertTruncatedTo(t, nested["note"], maxInputRunes)
	list, ok := v["list"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("list lost: %+v", v["list"])
	}
	if list[0] != "short" {
		t.Errorf("a short list entry must survive untouched: %+v", list[0])
	}
	assertTruncatedTo(t, list[1], maxInputRunes)
}

func assertTruncatedTo(t *testing.T, v any, n int) {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("not a string: %+v", v)
	}
	r := []rune(s)
	if len(r) != n+1 || r[len(r)-1] != '…' {
		t.Errorf("got %d runes ending %q, want %d plus the truncation mark", len(r), string(r[max(0, len(r)-1):]), n)
	}
}

// Numbers must survive a walk-and-remarshal exactly as written: encoding/json's
// default float64 both loses precision on a large id and can rewrite an
// ordinary integer in exponent form. Checked against the raw bytes, not by
// decoding them back into a plain map[string]any — THAT decode is exactly the
// lossy path (float64) this test exists to rule out inside truncateJSONStrings
// itself, so doing it again here would just hide the bug in the assertion.
func TestTruncateJSONStringsPreservesNumbersAndShortStrings(t *testing.T) {
	raw := json.RawMessage(`{"offset":42,"path":"/a/b.go","big":9007199254740993}`)
	got := truncateJSONStrings(raw)
	if !json.Valid(got) {
		t.Fatalf("not valid json: %s", got)
	}
	s := string(got)
	if !strings.Contains(s, `"path":"/a/b.go"`) {
		t.Errorf("path changed: %s", s)
	}
	if !strings.Contains(s, `"offset":42`) {
		t.Errorf("offset changed: %s", s)
	}
	if !strings.Contains(s, `"big":9007199254740993`) {
		t.Errorf("a big integer lost precision: %s", s)
	}
}

// Malformed or absent input must not be dropped — a tool call with input
// nobody could truncate is still worth showing as it arrived.
func TestTruncateJSONStringsPassesThroughWhatItCannotWalk(t *testing.T) {
	if got := truncateJSONStrings(nil); got != nil {
		t.Errorf("nil input: got %v", got)
	}
	broken := json.RawMessage(`{not json`)
	if got := truncateJSONStrings(broken); string(got) != string(broken) {
		t.Errorf("malformed input was altered: got %q, want %q", got, broken)
	}
}

// The transcript's footer says which brain answered and how long it took.
// Exercised directly against absorb, not Send: Send needs a live subprocess
// to drive, and absorb is the piece that actually sets these fields.
func TestAbsorbCapturesModelAndDuration(t *testing.T) {
	var assistant, result event
	mustLine(t, &assistant, `{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"hi"}]}}`)
	mustLine(t, &result, `{"type":"result","duration_ms":7830,"total_cost_usd":0.012,"result":"hi"}`)

	var turn Turn
	turn.absorb(assistant)
	turn.absorb(result)

	if turn.Model != "claude-opus-5" {
		t.Errorf("model not captured: %q", turn.Model)
	}
	if turn.Duration != 7830*time.Millisecond {
		t.Errorf("duration not captured: %s", turn.Duration)
	}
}

func mustLine(t *testing.T, into *event, line string) {
	t.Helper()
	if err := json.Unmarshal([]byte(line), into); err != nil {
		t.Fatalf("fixture is not json: %v", err)
	}
}
