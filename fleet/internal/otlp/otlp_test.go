package otlp

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

type sink struct {
	usage  []ledger.Usage
	events []ledger.Event
}

func (s *sink) AddUsage(u ledger.Usage) error { s.usage = append(s.usage, u); return nil }
func (s *sink) AddEvent(e ledger.Event) error { s.events = append(s.events, e); return nil }

const logs = `{"resourceLogs":[{"resource":{"attributes":[
  {"key":"fleet.agent","value":{"stringValue":"agent-03"}},{"key":"fleet.host","value":{"stringValue":"kali"}}]},
 "scopeLogs":[{"logRecords":[
  {"timeUnixNano":"1791427770000000000","body":{"stringValue":"claude_code.api_request"},"attributes":[
    {"key":"event.name","value":{"stringValue":"api_request"}},
    {"key":"model","value":{"stringValue":"claude-sonnet"}},
    {"key":"input_tokens","value":{"intValue":"120"}},
    {"key":"output_tokens","value":{"stringValue":"45"}},
    {"key":"cache_read_tokens","value":{"intValue":"9000"}},
    {"key":"cache_creation_tokens","value":{"intValue":"300"}},
    {"key":"cost_usd","value":{"doubleValue":0.0123}},
    {"key":"session.id","value":{"stringValue":"s1"}}]},
  {"timeUnixNano":"1791427771000000000","body":{"stringValue":"claude_code.api_error"},"attributes":[
    {"key":"error","value":{"stringValue":"rate limited"}},{"key":"status_code","value":{"stringValue":"429"}}]},
  {"timeUnixNano":"1791427772000000000","attributes":[
    {"key":"event.name","value":{"stringValue":"tool_result"}},{"key":"tool_name","value":{"stringValue":"Bash"}},
    {"key":"success","value":{"stringValue":"true"}}]}
 ]}]}]}`

func post(t *testing.T, h http.Handler, path, body, token string, gz bool) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Buffer
	if gz {
		rd = &bytes.Buffer{}
		w := gzip.NewWriter(rd)
		w.Write([]byte(body))
		w.Close()
	} else {
		rd = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(http.MethodPost, path, rd)
	req.Header.Set("Content-Type", "application/json")
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLogsBecomeUsageAndEvents(t *testing.T) {
	s := &sink{}
	rec := post(t, Handler(s, ""), "/v1/logs", logs, "", false)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(s.usage) != 1 {
		t.Fatalf("usage rows = %d", len(s.usage))
	}
	u := s.usage[0]
	if u.Agent != "agent-03" || u.Model != "claude-sonnet" || u.Input != 120 || u.Output != 45 ||
		u.CacheRead != 9000 || u.CacheCreation != 300 || u.CostUSD != 0.0123 || u.At.Unix() != 1791427770 {
		t.Errorf("usage = %+v", u)
	}
	var kinds []string
	for _, e := range s.events {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "otel.api_error,otel.tool_result" {
		t.Errorf("events = %v", kinds)
	}
	if !strings.Contains(s.events[0].Detail, "rate limited") {
		t.Errorf("api_error detail = %s", s.events[0].Detail)
	}
}

func TestGzippedBodies(t *testing.T) {
	s := &sink{}
	if rec := post(t, Handler(s, ""), "/v1/logs", logs, "", true); rec.Code != 200 || len(s.usage) != 1 {
		t.Fatalf("status %d usage %d", rec.Code, len(s.usage))
	}
}

func TestTokenIsRequiredWhenSet(t *testing.T) {
	s := &sink{}
	h := Handler(s, "secret")
	if rec := post(t, h, "/v1/logs", logs, "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := post(t, h, "/v1/logs", logs, "wrong", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	if rec := post(t, h, "/v1/logs", logs, "secret", false); rec.Code != 200 {
		t.Fatalf("right token: %d", rec.Code)
	}
}

// A record with no fleet.agent is not ours to account.
func TestRecordsWithoutAnAgentAreDropped(t *testing.T) {
	s := &sink{}
	body := strings.Replace(logs, `"fleet.agent"`, `"other"`, 1)
	if rec := post(t, Handler(s, ""), "/v1/logs", body, "", false); rec.Code != 200 || len(s.usage)+len(s.events) != 0 {
		t.Fatalf("status %d, kept %d rows", rec.Code, len(s.usage)+len(s.events))
	}
}

const metrics = `{"resourceMetrics":[{"resource":{"attributes":[{"key":"fleet.agent","value":{"stringValue":"agent-03"}}]},
 "scopeMetrics":[{"metrics":[
  {"name":"claude_code.commit.count","sum":{"dataPoints":[{"timeUnixNano":"1791427770000000000","asInt":"2"}]}},
  {"name":"claude_code.lines_of_code.count","sum":{"dataPoints":[
    {"timeUnixNano":"1791427770000000000","asInt":"40","attributes":[{"key":"type","value":{"stringValue":"added"}}]}]}},
  {"name":"claude_code.token.usage","sum":{"dataPoints":[{"timeUnixNano":"1791427770000000000","asDouble":5}]}}
 ]}]}]}`

// Counters the requests do not already carry are kept as events; token and
// cost counters are not, since api_request is the authoritative record.
func TestMetricsKeepTheActivityCounters(t *testing.T) {
	s := &sink{}
	if rec := post(t, Handler(s, ""), "/v1/metrics", metrics, "", false); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if len(s.events) != 2 || s.events[0].Kind != "metric.commit.count" || !strings.Contains(s.events[1].Detail, `"added"`) {
		t.Fatalf("events = %+v", s.events)
	}
}

// Claude Code's exporter (OTel-OTLP-Exporter-JavaScript) sends the header
// value undecoded: "Authorization: Bearer%20<token>". Captured on the wire.
func TestOTLPAcceptsAPercentEncodedBearer(t *testing.T) {
	s := &sink{}
	h := Handler(s, "secret")
	send := func(auth string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/logs", strings.NewReader(logs))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := send("Bearer%20secret"); c != 200 || len(s.usage) != 1 {
		t.Fatalf("encoded bearer: status %d, usage rows %d, want 200 and the api_request in usage", c, len(s.usage))
	}
	if c := send("Bearer secret"); c != 200 {
		t.Fatalf("plain bearer: %d", c)
	}
	for _, bad := range []string{"Bearer%20wrong", "Bearer wrong", "Bearer%20", "secret", "Bearer%20secret%20x", ""} {
		if c := send(bad); c != http.StatusUnauthorized {
			t.Errorf("%q: status %d, want 401", bad, c)
		}
	}
}

// toolLogs carries a tool decision and two results, each with the tool's
// parameters (OTEL_LOG_TOOL_DETAILS): a long Bash command, and an Edit's.
var toolLogs = `{"resourceLogs":[{"resource":{"attributes":[{"key":"fleet.agent","value":{"stringValue":"agent-03"}}]},
 "scopeLogs":[{"logRecords":[
  {"timeUnixNano":"1791427771000000000","attributes":[
    {"key":"event.name","value":{"stringValue":"tool_decision"}},{"key":"tool_name","value":{"stringValue":"Bash"}},
    {"key":"decision","value":{"stringValue":"accept"}},{"key":"session.id","value":{"stringValue":"s1"}}]},
  {"timeUnixNano":"1791427772000000000","attributes":[
    {"key":"event.name","value":{"stringValue":"tool_result"}},{"key":"tool_name","value":{"stringValue":"Bash"}},
    {"key":"success","value":{"stringValue":"true"}},{"key":"session.id","value":{"stringValue":"s1"}},
    {"key":"tool_parameters","value":{"stringValue":"{\"bash_command\":\"git\",\"full_command\":\"git push origin ` + strings.Repeat("x", 200) + `\",\"description\":\"push the branch\"}"}}]},
  {"timeUnixNano":"1791427773000000000","attributes":[
    {"key":"event.name","value":{"stringValue":"tool_result"}},{"key":"tool_name","value":{"stringValue":"Edit"}},
    {"key":"session.id","value":{"stringValue":"s1"}},
    {"key":"tool_parameters","value":{"stringValue":"{\"file_path\":\"/home/op/code/app/secret.go\"}"}},
    {"key":"tool_input","value":{"stringValue":"{\"old_string\":\"a\"}"}}]}
 ]}]}]}`

func TestToolEventsKeepTheToolAndABashCommandsStartOnly(t *testing.T) {
	s := &sink{}
	if rec := post(t, Handler(s, ""), "/v1/logs", toolLogs, "", false); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(s.events) != 3 || s.events[0].Kind != "otel.tool_decision" || s.events[1].Kind != "otel.tool_result" {
		t.Fatalf("events = %+v", s.events)
	}
	for _, e := range s.events {
		if e.Session != "s1" || !strings.Contains(e.Detail, `"tool_name"`) {
			t.Errorf("event = %+v", e)
		}
		for _, leak := range []string{"tool_parameters", "tool_input", "full_command", "description", "file_path", "secret.go", "old_string"} {
			if strings.Contains(e.Detail, leak) {
				t.Errorf("%s keeps %s: %s", e.Kind, leak, e.Detail)
			}
		}
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(s.events[1].Detail), &d); err != nil {
		t.Fatal(err)
	}
	if d["command"] != ("git push origin " + strings.Repeat("x", 200))[:120] || d["success"] != "true" {
		t.Errorf("bash detail = %v", d)
	}
	if strings.Contains(s.events[2].Detail, "command") {
		t.Errorf("an Edit has a command: %s", s.events[2].Detail)
	}
}

// sandboxes is a receiver's sandbox lookup: "key-09" is agent-09's key.
func sandboxes(bearer string) (string, bool) {
	if bearer == "key-09" {
		return "agent-09", true
	}
	return "", false
}

// A sandbox sends its telemetry under its sandbox key, and it is that
// agent's whatever the export says: a spoofed fleet.agent is not believed,
// though a slot of the key's own agent is kept.
func TestTelemetryWithASandboxKeyIsTheKeysAgents(t *testing.T) {
	s := &sink{}
	h := HandlerFor(s, "relay", sandboxes)
	if rec := post(t, h, "/v1/logs", logs, "key-09", false); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if len(s.usage) != 1 || s.usage[0].Agent != "agent-09" {
		t.Fatalf("usage = %+v, want agent-09's (the export says agent-03)", s.usage)
	}
	for _, e := range s.events {
		if e.Agent != "agent-09" {
			t.Errorf("event %s is %s's", e.Kind, e.Agent)
		}
	}
	slot := strings.Replace(logs, `"agent-03"`, `"agent-09:2"`, 1)
	if rec := post(t, h, "/v1/logs", slot, "key-09", false); rec.Code != 200 || s.usage[1].Agent != "agent-09:2" {
		t.Errorf("a slot of its own: %d, %+v", rec.Code, s.usage)
	}
	missing := strings.Replace(logs, `{"key":"fleet.agent","value":{"stringValue":"agent-03"}},`, "", 1)
	if rec := post(t, h, "/v1/logs", missing, "key-09", false); rec.Code != 200 || s.usage[2].Agent != "agent-09" {
		t.Errorf("no fleet.agent: %d, %+v", rec.Code, s.usage)
	}
	// The relay token still says nothing about who sends.
	if rec := post(t, h, "/v1/logs", logs, "relay", false); rec.Code != 200 || s.usage[3].Agent != "agent-03" {
		t.Errorf("relay: %d, %+v", rec.Code, s.usage)
	}
	if rec := post(t, h, "/v1/metrics", `{"resourceMetrics":[]}`, "key-10", false); rec.Code != 401 {
		t.Errorf("another key: %d, want 401", rec.Code)
	}
}
