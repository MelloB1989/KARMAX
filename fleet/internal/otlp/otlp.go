// Package otlp receives the agents' OpenTelemetry export (OTLP/HTTP, JSON
// encoding): api_request logs are the authoritative token and cost account,
// errors and tool results go to the event log, and the activity counters
// (commits, PRs, lines, active time, sessions) are kept as events.
//
// Prompts, responses and tool parameters are redacted by Claude Code's
// default and stay that way; the transcript is the full record.
package otlp

import (
	"compress/gzip"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// Sink is where records go.
type Sink interface {
	AddUsage(ledger.Usage) error
	AddEvent(ledger.Event) error
}

// Handler serves /v1/logs and /v1/metrics. A non-empty token is required as
// a bearer token (set through OTEL_EXPORTER_OTLP_HEADERS in the agents).
func Handler(s Sink, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/logs", func(w http.ResponseWriter, r *http.Request) {
		var req logsRequest
		if !decode(w, r, token, &req) {
			return
		}
		ingestLogs(s, &req)
		ok(w)
	})
	mux.HandleFunc("POST /v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		var req metricsRequest
		if !decode(w, r, token, &req) {
			return
		}
		ingestMetrics(s, &req)
		ok(w)
	})
	mux.HandleFunc("POST /v1/traces", func(w http.ResponseWriter, r *http.Request) {
		if authorized(r, token) {
			ok(w)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	return mux
}

func authorized(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func decode(w http.ResponseWriter, r *http.Request, token string, v any) bool {
	if !authorized(r, token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, 16<<20)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return false
		}
		defer gz.Close()
		body = io.LimitReader(gz, 64<<20)
	}
	if !strings.Contains(r.Header.Get("Content-Type"), "json") {
		http.Error(w, "only OTLP/HTTP JSON is accepted (OTEL_EXPORTER_OTLP_PROTOCOL=http/json)", http.StatusUnsupportedMediaType)
		return false
	}
	if err := json.NewDecoder(body).Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func ok(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

type anyValue struct {
	StringValue *string  `json:"stringValue"`
	IntValue    any      `json:"intValue"` // int64 is a JSON string in OTLP/JSON
	DoubleValue *float64 `json:"doubleValue"`
	BoolValue   *bool    `json:"boolValue"`
}

func (v anyValue) String() string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.IntValue != nil:
		return strings.Trim(toString(v.IntValue), `"`)
	case v.DoubleValue != nil:
		return strconv.FormatFloat(*v.DoubleValue, 'f', -1, 64)
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	}
	return ""
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type attrs []keyValue

func (a attrs) get(k string) string {
	for _, kv := range a {
		if kv.Key == k {
			return kv.Value.String()
		}
	}
	return ""
}

func (a attrs) int(k string) int64 {
	f, _ := strconv.ParseFloat(a.get(k), 64)
	return int64(f)
}

func (a attrs) float(k string) float64 {
	f, _ := strconv.ParseFloat(a.get(k), 64)
	return f
}

func (a attrs) asMap() map[string]string {
	m := make(map[string]string, len(a))
	for _, kv := range a {
		m[kv.Key] = kv.Value.String()
	}
	return m
}

func nanos(s string) time.Time {
	n, err := strconv.ParseInt(strings.Trim(s, `"`), 10, 64)
	if err != nil || n <= 0 {
		return time.Now()
	}
	return time.Unix(0, n)
}

type logsRequest struct {
	ResourceLogs []struct {
		Resource struct {
			Attributes attrs `json:"attributes"`
		} `json:"resource"`
		ScopeLogs []struct {
			LogRecords []struct {
				TimeUnixNano         string   `json:"timeUnixNano"`
				ObservedTimeUnixNano string   `json:"observedTimeUnixNano"`
				Body                 anyValue `json:"body"`
				Attributes           attrs    `json:"attributes"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

func ingestLogs(s Sink, req *logsRequest) {
	for _, rl := range req.ResourceLogs {
		agent := rl.Resource.Attributes.get("fleet.agent")
		if agent == "" {
			continue
		}
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				a := lr.Attributes
				name := a.get("event.name")
				if name == "" {
					name = lr.Body.String()
				}
				name = strings.TrimPrefix(name, "claude_code.")
				ts := lr.TimeUnixNano
				if ts == "" || ts == "0" {
					ts = lr.ObservedTimeUnixNano
				}
				at := nanos(ts)
				switch name {
				case "api_request":
					_ = s.AddUsage(ledger.Usage{
						Agent: agent, Model: a.get("model"), At: at,
						Input: a.int("input_tokens"), Output: a.int("output_tokens"),
						CacheRead: a.int("cache_read_tokens"), CacheCreation: a.int("cache_creation_tokens"),
						CostUSD: a.float("cost_usd"),
					})
				case "api_error", "tool_result":
					detail, _ := json.Marshal(a.asMap())
					_ = s.AddEvent(ledger.Event{Agent: agent, At: at, Kind: "otel." + name,
						Session: a.get("session.id"), Detail: string(detail)})
				}
			}
		}
	}
}

type dataPoint struct {
	TimeUnixNano string   `json:"timeUnixNano"`
	AsInt        any      `json:"asInt"`
	AsDouble     *float64 `json:"asDouble"`
	Attributes   attrs    `json:"attributes"`
}

type metricsRequest struct {
	ResourceMetrics []struct {
		Resource struct {
			Attributes attrs `json:"attributes"`
		} `json:"resource"`
		ScopeMetrics []struct {
			Metrics []struct {
				Name string `json:"name"`
				Sum  *struct {
					DataPoints             []dataPoint `json:"dataPoints"`
					AggregationTemporality any         `json:"aggregationTemporality"`
				} `json:"sum"`
			} `json:"metrics"`
		} `json:"scopeMetrics"`
	} `json:"resourceMetrics"`
}

// counters are the activity metrics worth keeping; token and cost counters
// duplicate api_request and are not.
var counters = map[string]bool{
	"session.count": true, "lines_of_code.count": true, "commit.count": true,
	"pull_request.count": true, "active_time.total": true,
}

func ingestMetrics(s Sink, req *metricsRequest) {
	for _, rm := range req.ResourceMetrics {
		agent := rm.Resource.Attributes.get("fleet.agent")
		if agent == "" {
			continue
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				name := strings.TrimPrefix(m.Name, "claude_code.")
				if !counters[name] || m.Sum == nil {
					continue
				}
				for _, dp := range m.Sum.DataPoints {
					v := 0.0
					if dp.AsDouble != nil {
						v = *dp.AsDouble
					} else if dp.AsInt != nil {
						v, _ = strconv.ParseFloat(strings.Trim(toString(dp.AsInt), `"`), 64)
					}
					detail, _ := json.Marshal(map[string]any{"value": v, "attributes": dp.Attributes.asMap(),
						"temporality": m.Sum.AggregationTemporality})
					_ = s.AddEvent(ledger.Event{Agent: agent, At: nanos(dp.TimeUnixNano), Kind: "metric." + name,
						Session: dp.Attributes.get("session.id"), Detail: string(detail)})
				}
			}
		}
	}
}
