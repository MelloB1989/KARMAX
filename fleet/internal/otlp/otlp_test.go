package otlp

import (
	"bytes"
	"compress/gzip"
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
