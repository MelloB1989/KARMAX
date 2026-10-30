package posthog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

var clock = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

// fakePostHog is a PostHog instance with projects, exception counts, alerts
// and daily event counts.
type fakePostHog struct {
	t        *testing.T
	mu       sync.Mutex
	srv      *httptest.Server
	key      string
	projects []map[string]any
	errors   map[string][][]any // project id -> rows
	alerts   map[string][]map[string]any
	daily    map[string][][]any
	queries  []string // HogQL received, in order
	paths    []string
	// noAlertScope answers the alerts API 403, as for a key without alert:read.
	noAlertScope bool
}

var errorColumns = []string{"issue", "recent", "before", "first_seen", "issue_id", "exc_type", "exc_message", "types", "vals"}

func newFakePostHog(t *testing.T) *fakePostHog {
	f := &fakePostHog{t: t, key: "phx_test_key", errors: map[string][][]any{}, alerts: map[string][]map[string]any{},
		daily: map[string][][]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePostHog) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+f.key {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"authentication_error","detail":"Invalid personal API key."}`))
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // api projects {id} {what}
	switch {
	case len(parts) == 2 && parts[1] == "projects":
		_ = json.NewEncoder(w).Encode(map[string]any{"results": f.projects, "next": nil})
	case len(parts) == 3 && parts[1] == "projects":
		for _, p := range f.projects {
			if jsonID(p["id"]) == parts[2] {
				_ = json.NewEncoder(w).Encode(p)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case len(parts) == 4 && parts[3] == "query":
		var body struct {
			Query struct {
				Kind  string `json:"kind"`
				Query string `json:"query"`
			} `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.queries = append(f.queries, body.Query.Query)
		if body.Query.Kind != "HogQLQuery" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.Contains(body.Query.Query, "$exception") {
			_ = json.NewEncoder(w).Encode(map[string]any{"columns": errorColumns, "results": f.errors[parts[2]]})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"columns": []string{"event", "y", "prior"}, "results": f.daily[parts[2]]})
	case len(parts) == 4 && parts[3] == "alerts":
		if f.noAlertScope {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"type":"authentication_error","code":"permission_denied"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": f.alerts[parts[2]]})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func jsonID(v any) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

func (f *fakePostHog) count(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, q := range f.queries {
		if strings.Contains(q, substr) {
			n++
		}
	}
	return n
}

// ttlKV is the store's key-value table, with expiry on the source's clock.
type ttlKV struct {
	mu  sync.Mutex
	now *time.Time
	m   map[string]string
	exp map[string]time.Time
}

func newKV(now *time.Time) *ttlKV {
	return &ttlKV{now: now, m: map[string]string{}, exp: map[string]time.Time{}}
}

func (k *ttlKV) KVGet(group, key string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	id := group + "\x00" + key
	if e, ok := k.exp[id]; ok && !k.now.Before(e) {
		return "", false, nil
	}
	v, ok := k.m[id]
	return v, ok, nil
}

func (k *ttlKV) KVSet(group, key, value string, ttl time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	id := group + "\x00" + key
	k.m[id] = value
	delete(k.exp, id)
	if ttl > 0 {
		k.exp[id] = k.now.Add(ttl)
	}
	return nil
}

type harness struct {
	f   *fakePostHog
	now time.Time
	kv  *ttlKV
	src *Source
	got []sources.Event
	acc []Account
}

func newHarness(t *testing.T, projects ...string) *harness {
	h := &harness{f: newFakePostHog(t), now: clock}
	h.kv = newKV(&h.now)
	h.f.projects = []map[string]any{{"id": 101, "name": "Storefront"}, {"id": 202, "name": "Back Office"}}
	h.src = New(Config{}, h.kv, func(e sources.Event) error { h.got = append(h.got, e); return nil })
	h.src.Now = func() time.Time { return h.now }
	h.acc = []Account{{Integration: "posthog", APIKey: h.f.key, Host: h.f.srv.URL, ProjectIDs: projects}}
	return h
}

func (h *harness) tick(t *testing.T) Result {
	t.Helper()
	res, err := h.src.Tick(context.Background(), h.acc)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (h *harness) ofType(typ string) []sources.Event {
	var out []sources.Event
	for _, e := range h.got {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// An exception happening far more than it usually does is urgent; one never
// seen before is news; one at its usual rate is nothing.
func TestASpikingErrorIsUrgentANewOneIsNewsAndTheUsualIsNothing(t *testing.T) {
	h := newHarness(t, "101")
	h.f.errors["101"] = [][]any{
		// 120 in the last hour against 168 over the week before: ~1 an hour.
		{"iss-a", 120, 168, "2026-10-20T10:00:00Z", "iss-a", "TypeError", "cart is undefined", nil, nil},
		// Never seen before this hour.
		{"iss-b", 2, 0, "2026-10-31T08:40:00Z", "iss-b", nil, nil, `["RangeError"]`, `["page out of range"]`},
		// At its usual rate.
		{"iss-c", 3, 500, "2026-09-01T00:00:00Z", "iss-c", "Error", "timeout", nil, nil},
	}
	h.tick(t)

	spikes, fresh := h.ofType("error.spike"), h.ofType("error.new")
	if len(spikes) != 1 || len(fresh) != 1 || len(h.got) != 2 {
		t.Fatalf("got %d events (%d spikes, %d new), want one of each: %+v", len(h.got), len(spikes), len(fresh), h.got)
	}
	sp, nw := spikes[0], fresh[0]
	if !sp.Urgent || nw.Urgent {
		t.Errorf("urgent spike=%v new=%v, want true false", sp.Urgent, nw.Urgent)
	}
	if sp.Source != "posthog" || sp.Account != "Storefront" {
		t.Errorf("spike from %s:%s, want posthog:Storefront", sp.Source, sp.Account)
	}
	if !strings.Contains(sp.Title, "TypeError") || !strings.Contains(sp.Title, "120") {
		t.Errorf("spike title %q should name the error and the count", sp.Title)
	}
	if !strings.Contains(nw.Title, "RangeError") || !strings.Contains(nw.Title, "page out of range") {
		t.Errorf("new-error title %q should read the exception from its lists", nw.Title)
	}
	if want := h.f.srv.URL + "/project/101/error_tracking/iss-a"; sp.URL != want {
		t.Errorf("spike url %q, want %q", sp.URL, want)
	}
	if q := h.f.queries[0]; !strings.Contains(q, "event = '$exception'") || !strings.Contains(q, "3600") {
		t.Errorf("the exception query does not read the last hour of $exception events: %s", q)
	}
}

// A spike is told once, not every tick it stays high; the next one after the
// cool-down is told again. A new error is new once.
func TestAnErrorIsToldOncePerCooldown(t *testing.T) {
	h := newHarness(t, "101")
	h.f.errors["101"] = [][]any{
		{"iss-a", 120, 168, "2026-10-20T10:00:00Z", "iss-a", "TypeError", "cart is undefined", nil, nil},
		{"iss-b", 2, 0, "2026-10-31T08:40:00Z", "iss-b", "RangeError", "page out of range", nil, nil},
	}
	h.tick(t)
	h.tick(t) // within ErrorsEvery: not even asked
	if n := h.f.count("$exception"); n != 1 {
		t.Errorf("exceptions queried %d times in one interval", n)
	}
	h.now = h.now.Add(20 * time.Minute)
	h.tick(t)
	if len(h.got) != 2 {
		t.Fatalf("after another check %d events, want the first 2 only", len(h.got))
	}
	h.now = h.now.Add(7 * time.Hour)
	h.tick(t)
	if len(h.ofType("error.spike")) != 2 || len(h.ofType("error.new")) != 1 {
		t.Errorf("after the cool-down: %d spikes, %d new; want 2 and 1", len(h.ofType("error.spike")), len(h.ofType("error.new")))
	}
}

// An insight alert going off is urgent, once per time it goes off.
func TestAFiringAlertIsUrgentOncePerEpisode(t *testing.T) {
	h := newHarness(t, "101")
	alert := map[string]any{"id": "al-1", "name": "Checkout conversion below 2%", "state": "Firing", "enabled": true,
		"insight": map[string]any{"id": 7, "short_id": "Ab12Cd", "name": "Checkout funnel"}}
	h.f.alerts["101"] = []map[string]any{alert, {"id": "al-2", "name": "Signups", "state": "Not firing", "enabled": true, "insight": 9}}

	h.tick(t)
	h.tick(t)
	firing := h.ofType("alert.firing")
	if len(firing) != 1 {
		t.Fatalf("%d firing events, want 1", len(firing))
	}
	e := firing[0]
	if !e.Urgent || !strings.Contains(e.Title, "Checkout conversion below 2%") || e.Account != "Storefront" {
		t.Errorf("alert event %+v", e)
	}
	if want := h.f.srv.URL + "/project/101/insights/Ab12Cd?alert_id=al-1"; e.URL != want {
		t.Errorf("alert url %q, want %q", e.URL, want)
	}

	alert["state"] = "Not firing"
	h.now = h.now.Add(10 * time.Minute)
	h.tick(t)
	alert["state"] = "Firing"
	h.now = h.now.Add(10 * time.Minute)
	h.tick(t)
	if n := len(h.ofType("alert.firing")); n != 2 {
		t.Errorf("an alert that stopped and fired again was told %d times, want 2", n)
	}
}

// With no project ids configured, every project the key can see is watched,
// each as its own account.
func TestProjectsAreDiscoveredWhenNoneAreConfigured(t *testing.T) {
	h := newHarness(t)
	h.f.alerts["101"] = []map[string]any{{"id": "al-1", "name": "A", "state": "Firing", "insight": map[string]any{"short_id": "x"}}}
	h.f.alerts["202"] = []map[string]any{{"id": "al-9", "name": "B", "state": "Firing", "insight": map[string]any{"short_id": "y"}}}
	h.tick(t)
	accounts := map[string]bool{}
	for _, e := range h.got {
		accounts[e.Account] = true
	}
	if !accounts["Storefront"] || !accounts["Back Office"] {
		t.Errorf("accounts %v, want both discovered projects", accounts)
	}
}

// Once a day, the main events are set beside their week: a large move is
// worth deciding on, an ordinary day is not.
func TestADailyMoveInAKeyMetricIsNoticedOncePerDay(t *testing.T) {
	h := newHarness(t, "101")
	h.f.daily["101"] = [][]any{
		{"signed_up", 30, 840},    // ~120 a day, 30 yesterday
		{"$pageview", 1000, 7000}, // its usual day
	}
	h.tick(t)
	h.now = h.now.Add(time.Hour)
	h.tick(t)
	got := h.ofType("metric.anomaly")
	if len(got) != 1 {
		t.Fatalf("%d anomalies, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Title, "signed_up") || !strings.Contains(got[0].Title, "75%") {
		t.Errorf("anomaly title %q should say what moved and how much", got[0].Title)
	}
	if n := h.f.count("yesterday()"); n != 1 {
		t.Errorf("the daily check ran %d times in a day", n)
	}

	// Named metrics replace the automatic top events.
	h2 := newHarness(t, "101")
	h2.src.cfg.Metrics = []string{"signed_up", "it's quoted"}
	h2.tick(t)
	if q := h2.f.queries[len(h2.f.queries)-1]; !strings.Contains(q, `event IN ('signed_up', 'it\'s quoted')`) {
		t.Errorf("named metrics not queried safely: %s", q)
	}
}

// With no key the source is off and says what to run; a refused key is an
// error that never repeats the key.
func TestWithoutAKeyTheSourceIsOffAndSaysWhatToRun(t *testing.T) {
	h := newHarness(t)
	res, err := h.src.Tick(context.Background(), []Account{{Integration: "posthog"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"karmax login posthog", "posthog_api_key", "query:read"} {
		if !strings.Contains(res.Off, want) {
			t.Errorf("off reason %q does not say %q", res.Off, want)
		}
	}
	if len(h.f.paths) != 0 {
		t.Error("an unconfigured source called PostHog")
	}

	_, err = h.src.Tick(context.Background(), []Account{{Integration: "posthog", APIKey: "phx_wrong_secret", Host: h.f.srv.URL, ProjectIDs: []string{"101"}}})
	if err == nil || strings.Contains(err.Error(), "phx_wrong_secret") {
		t.Errorf("a refused key: err = %v", err)
	}
	if err := Health(context.Background(), Account{APIKey: "phx_wrong_secret", Host: h.f.srv.URL}); err == nil || !strings.Contains(err.Error(), "karmax login posthog") {
		t.Errorf("health on a refused key should say how to fix it: %v", err)
	}
	if err := Health(context.Background(), Account{APIKey: h.f.key, Host: h.f.srv.URL}); err != nil {
		t.Errorf("health on a good key: %v", err)
	}
}

// The capture host people copy from their snippet is not the API host.
func TestTheHostIsTheAPIHost(t *testing.T) {
	for in, want := range map[string]string{
		"":                          "https://us.posthog.com",
		"eu.i.posthog.com":          "https://eu.posthog.com",
		"https://us.i.posthog.com/": "https://us.posthog.com",
		"https://ph.example.test":   "https://ph.example.test",
	} {
		if got := APIHost(in); got != want {
			t.Errorf("APIHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// A key without alert:read still watches errors; the gap is reported, not a
// failure every poll.
func TestAKeyWithoutAlertScopeStillWatchesErrors(t *testing.T) {
	h := newHarness(t, "101")
	h.f.noAlertScope = true
	h.f.errors["101"] = [][]any{{"iss-a", 120, 168, "2026-10-20T10:00:00Z", "iss-a", "TypeError", "cart is undefined", nil, nil}}
	h.tick(t)
	if len(h.ofType("error.spike")) != 1 {
		t.Fatalf("errors were not watched: %+v", h.got)
	}
	st := h.src.Status()
	if len(st) != 1 || !strings.Contains(st[0].Note, "alert:read") {
		t.Errorf("status %+v should say alerts need alert:read", st)
	}
}
