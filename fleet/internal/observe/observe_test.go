package observe

import (
	"strings"
	"testing"
	"time"
)

// The real 2.1.294 shape: interactive rows carry pid and status, background
// rows carry id and state.
const agentsJSON = `[
 {"id":"340b8b2d","cwd":"/x","kind":"background","startedAt":1785815235897,"sessionId":"340b8b2d-8d09","name":"bg-one","state":"blocked"},
 {"pid":20211,"cwd":"/work","kind":"interactive","startedAt":1791427770493,"sessionId":"7e796528-b56a","name":"agent-03","status":"idle"},
 {"pid":20300,"cwd":"/work","kind":"interactive","startedAt":1791427790000,"sessionId":"aaaa-bbbb","name":"stray","status":"busy"}
]`

func TestParseAgents(t *testing.T) {
	got, err := ParseAgents([]byte(agentsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows", len(got))
	}
	a := got[1]
	if a.Name != "agent-03" || a.PID != 20211 || a.Status != "idle" || a.SessionID != "7e796528-b56a" || !a.Interactive() {
		t.Errorf("row = %+v", a)
	}
	if a.Started().UnixMilli() != 1791427770493 {
		t.Errorf("started = %v", a.Started())
	}
	if got[0].Interactive() || got[0].Activity() != "blocked" || got[1].Activity() != "idle" {
		t.Errorf("activity: %q %q", got[0].Activity(), got[1].Activity())
	}
}

func TestParseAgentsToleratesNoiseBeforeTheArray(t *testing.T) {
	got, err := ParseAgents([]byte("warning: something\n" + agentsJSON))
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d, %v", len(got), err)
	}
}

func TestParseStatusReadsTheAccountsWindows(t *testing.T) {
	snap := `{"session_id":"s1","model":{"id":"claude-sonnet","display_name":"Sonnet"},
	 "context_window":{"used_percentage":41.5},"cost":{"total_cost_usd":1.25},
	 "rate_limits":{"five_hour":{"used_percentage":82,"resets_at":1791430000},
	                "seven_day":{"used_percentage":12.5,"resets_at":"2026-10-12T00:00:00Z"}}}`
	s, err := ParseStatus([]byte(snap))
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionID != "s1" || s.Model.ID != "claude-sonnet" || s.ContextPct() != 41.5 || s.Cost.TotalCostUSD != 1.25 {
		t.Errorf("status = %+v", s)
	}
	five, ok := s.FiveHour()
	if !ok || five.Used != 82 || !five.Resets.Equal(time.Unix(1791430000, 0)) {
		t.Errorf("five_hour = %+v %v", five, ok)
	}
	seven, ok := s.SevenDay()
	if !ok || seven.Used != 12.5 || seven.Resets.Format(time.RFC3339) != "2026-10-12T00:00:00Z" {
		t.Errorf("seven_day = %+v %v", seven, ok)
	}
}

// A freshly rotated session has made no API call yet, so it has no windows.
func TestParseStatusWithoutRateLimits(t *testing.T) {
	s, err := ParseStatus([]byte(`{"session_id":"s2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.FiveHour(); ok {
		t.Error("no rate_limits must mean no window, not a zero one")
	}
}

func TestResetsAtInMilliseconds(t *testing.T) {
	s, _ := ParseStatus([]byte(`{"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":1791430000000}}}`))
	if w, _ := s.FiveHour(); !w.Resets.Equal(time.Unix(1791430000, 0)) {
		t.Errorf("ms resets_at read as %v", w.Resets)
	}
}

func TestParseEvents(t *testing.T) {
	lines := `{"t":1791427770,"event":"SessionStart","session":"s1"}
not json
{"t":1791427780,"event":"PostToolUse","session":"s1","tool":"Bash","ok":false}
{"t":1791427790,"event":"StopFailure","session":"s1","raw":{"hook_event_name":"StopFailure","error_type":"rate_limit"}}
`
	evs, err := ParseEvents(strings.NewReader(lines))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("got %d events", len(evs))
	}
	if evs[1].Tool != "Bash" || evs[1].OK == nil || *evs[1].OK {
		t.Errorf("tool event = %+v", evs[1])
	}
	if evs[2].Event != "StopFailure" || evs[2].ErrorType() != "rate_limit" {
		t.Errorf("stopfailure type = %q", evs[2].ErrorType())
	}
}

// The StopFailure payload's field name is unconfirmed (spike S4), so the error
// type is looked for under each plausible key, and as a last resort found by
// its value anywhere in the payload.
func TestStopFailureErrorTypeUnderOtherKeys(t *testing.T) {
	for _, raw := range []string{
		`{"error":"authentication_failed"}`,
		`{"reason":"billing_error"}`,
		`{"error":{"type":"account_on_hold"}}`,
		`{"details":{"kind":"rate_limit"}}`,
	} {
		evs, _ := ParseEvents(strings.NewReader(`{"t":1,"event":"StopFailure","raw":` + raw + "}\n"))
		if len(evs) != 1 || evs[0].ErrorType() == "" {
			t.Errorf("no error type found in %s", raw)
		}
	}
}

const transcript = `{"type":"user","sessionId":"s1","cwd":"/work","timestamp":"2026-10-08T10:00:00Z","turnOrigin":"peer","message":{"role":"user","content":"<cross-session-message from-name=\"karmax\">fix the bug</cross-session-message>"}}
{"type":"assistant","sessionId":"s1","timestamp":"2026-10-08T10:00:05Z","message":{"id":"m1","model":"claude-sonnet","content":[{"type":"text","text":"looking"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":50}}}
{"type":"assistant","sessionId":"s1","timestamp":"2026-10-08T10:00:06Z","message":{"id":"m1","model":"claude-sonnet","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":50}}}
{"type":"user","sessionId":"s1","timestamp":"2026-10-08T10:00:07Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"type":"assistant","sessionId":"s1","timestamp":"2026-10-08T10:00:09Z","message":{"id":"m2","model":"claude-sonnet","content":[{"type":"text","text":"done, pushed fleet/agent-03/bug"}],"usage":{"input_tokens":3,"output_tokens":7}}}
{"type":"user","sessionId":"s1","timestamp":"2026-10-08T10:05:00Z","turnOrigin":"human","message":{"role":"user","content":[{"type":"text","text":"thanks"}]}}
{"type":"cost-state","sessionId":"s1","totalCostUSD":0.42}
garbage
`

func TestSummarizeTranscript(t *testing.T) {
	s, err := Summarize(strings.NewReader(transcript))
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionID != "s1" || s.Cwd != "/work" {
		t.Errorf("ids: %+v", s)
	}
	if s.Turns != 2 || s.PeerMessages != 1 {
		t.Errorf("turns=%d peer=%d, want 2 prompts of which 1 from a peer", s.Turns, s.PeerMessages)
	}
	// m1 is two lines (one per content block) with one usage: counted once.
	if s.Usage.Input != 13 || s.Usage.Output != 12 || s.Usage.CacheRead != 100 || s.Usage.CacheCreation != 50 {
		t.Errorf("usage = %+v", s.Usage)
	}
	if s.CostUSD != 0.42 || s.Models[0] != "claude-sonnet" {
		t.Errorf("cost=%v models=%v", s.CostUSD, s.Models)
	}
	if s.LastAssistant != "done, pushed fleet/agent-03/bug" {
		t.Errorf("last = %q", s.LastAssistant)
	}
	if s.Started.Format(time.RFC3339) != "2026-10-08T10:00:00Z" || s.Ended.Format(time.RFC3339) != "2026-10-08T10:05:00Z" {
		t.Errorf("span %v .. %v", s.Started, s.Ended)
	}
}

func TestProjectKey(t *testing.T) {
	if got := ProjectKey("/home/op/.karmax/sessions/agent_nexus"); got != "-home-op--karmax-sessions-agent-nexus" {
		t.Errorf("ProjectKey = %q", got)
	}
}
