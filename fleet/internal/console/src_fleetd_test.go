package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	statusJSON = `{"agents":[
	 {"agent":"agent-01","host":"pc2","state":"working","task":"T1","idle_s":4,"model":"sonnet","alerts":["stuck"],"models":["sonnet"],"session":"s1","recorded_at":"2026-10-11T14:00:00Z"},
	 {"agent":"agent-02","host":"pc2","state":"standby","idle_s":900,"models":[],"recorded_at":"2026-10-11T14:00:00Z"}],
	 "orchestrator":{"FiveHour":12.5,"SevenDay":30,"FiveHourResets":"2026-10-11T18:00:00Z","SevenDayResets":"2026-10-15T00:00:00Z"}}`
	tasksOpen  = `[{"id":"T1","agent":"agent-01","assigner":"operator","assigned":"2026-10-11T13:00:00Z","closed":null,"outcome":""}]`
	eventsJSON = `[{"id":7,"agent":"agent-01","at":"2026-10-11T13:59:00Z","kind":"Stop","session":"s1","detail":{"x":1}},
	               {"id":8,"agent":"agent-01","at":"2026-10-11T13:59:30Z","kind":"fleet.relay","detail":null}]`
)

func writeToken(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "fleetd.token")
	if err := os.WriteFile(p, []byte("secret-token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func fleetdServer(t *testing.T, fail *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token-value" {
			http.Error(w, "no", 401)
			return
		}
		if fail != nil && fail.Load() > 0 && r.URL.Path == "/v1/status" {
			fail.Add(-1)
			http.Error(w, "fleetd is restarting", 503)
			return
		}
		switch r.URL.Path {
		case "/v1/status":
			w.Write([]byte(statusJSON))
		case "/v1/tasks":
			switch {
			case r.URL.Query().Get("open") == "1":
				w.Write([]byte(tasksOpen))
			case r.URL.Query().Get("agent") == "agent-01":
				w.Write([]byte(`[{"id":"T0","agent":"agent-01","assigner":"operator","assigned":"2026-10-11T10:00:00Z","closed":"2026-10-11T12:00:00Z","outcome":"done"}]`))
			default:
				w.Write([]byte(`[]`))
			}
		case "/v1/history":
			w.Write([]byte(`[{"ID":"x","Agent":"agent-01","Task":"T0","CostUSD":1.25},{"ID":"y","Agent":"agent-01","Task":"T0","CostUSD":0.5},{"ID":"z","Task":"other","CostUSD":9}]`))
		case "/v1/events":
			if r.URL.Query().Get("after") == "0" {
				w.Write([]byte(eventsJSON))
			} else {
				w.Write([]byte(`[]`))
			}
		case "/v1/activity":
			w.Write([]byte(`{"bucket_s":900,"agents":{"agent-01":[{"t":"2026-10-11T13:45:00Z","n":3}]}}`))
		case "/v1/today":
			w.Write([]byte(`{"since":"2026-10-11T00:00:00Z","tasks_closed":2,"spend_usd":3.5,"output_tokens":12000}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestFleetdFillsRecordedFields(t *testing.T) {
	srv := fleetdServer(t, nil)
	defer srv.Close()
	now := time.Date(2026, 10, 11, 14, 0, 5, 0, time.UTC)
	f := newFleetdSrc(NewFleetdClient(srv.URL, writeToken(t)), func() time.Time { return now }, "karmax", []string{"agent-01", "agent-02"})
	ctx := context.Background()
	if err := f.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.pollActivity(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.pollToday(ctx); err != nil {
		t.Fatal(err)
	}
	if src := f.source(now); !src.OK || src.Error != "" {
		t.Fatalf("source = %+v", src)
	}
	if f.cursor != 8 || len(f.events) != 2 || f.events[0].ID != 7 {
		t.Fatalf("events/cursor = %+v / %d", f.events, f.cursor)
	}
	if err := f.poll(ctx); err != nil || len(f.events) != 2 {
		t.Fatalf("second poll must add no events: %v %d", err, len(f.events))
	}
	r := f.rows["agent-01"]
	if r.State != "working" || r.IdleS != 4 || r.Alerts[0] != "stuck" || r.RecordedAt.IsZero() {
		t.Fatalf("row = %+v", r)
	}
	if f.orch == nil || f.orch.FiveHourPct != 12.5 || f.orch.Name != "karmax" {
		t.Fatalf("orchestrator = %+v", f.orch)
	}
	if len(f.tasks) != 1 || f.tasks[0].Assigner != "operator" {
		t.Fatalf("tasks = %+v", f.tasks)
	}
	if f.today.TasksClosed != 2 || f.today.SpendUSD != 3.5 {
		t.Fatalf("today = %+v", f.today)
	}
	if lt := f.lastTask["agent-01"]; lt == nil || lt.Outcome != "done" || lt.CostUSD != 1.75 {
		t.Fatalf("last task = %+v (cost is the sessions of that task only)", lt)
	}
	if f.lastTask["agent-02"] != nil || len(f.activity["agent-01"]) != 1 {
		t.Fatalf("last %v activity %v", f.lastTask, f.activity)
	}
}

func TestFleetdOutageMarksStaleThenRecovers(t *testing.T) {
	var fail atomic.Int32
	srv := fleetdServer(t, &fail)
	defer srv.Close()
	clock := time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC)
	f := newFleetdSrc(NewFleetdClient(srv.URL, writeToken(t)), func() time.Time { return clock }, "karmax", nil)
	ctx := context.Background()
	if err := f.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.tr.source(clock).OK {
		t.Fatal("not ok after a good poll")
	}
	fail.Store(3)
	for range 3 {
		clock = clock.Add(5 * time.Second)
		if err := f.poll(ctx); err == nil {
			t.Fatal("want a 503 error")
		} else if strings.Contains(err.Error(), "secret-token-value") {
			t.Fatal("the token leaked into an error")
		}
	}
	src := f.tr.source(clock) // 15 s since the last success
	if src.OK || !f.tr.stale(clock) || !strings.Contains(src.Error, "503") {
		t.Fatalf("during the outage: %+v stale=%v", src, f.tr.stale(clock))
	}
	if f.rows["agent-01"].State != "working" {
		t.Fatal("the last good values must stay through the outage")
	}
	clock = clock.Add(5 * time.Second)
	if err := f.poll(ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if src := f.tr.source(clock); !src.OK || src.Error != "" {
		t.Fatalf("after recovery: %+v", src)
	}
	if f.tr.stale(clock) {
		t.Fatal("stale after recovery")
	}
	if f.cursor != 8 || len(f.events) != 2 {
		t.Fatalf("events must not duplicate or gap across the outage: %d %d", f.cursor, len(f.events))
	}
}

func TestFleetdMissingRouteDoesNotBlankTheRoster(t *testing.T) {
	// a fleetd that predates /v1/tasks, /v1/today and /v1/activity
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			w.Write([]byte(statusJSON))
		case "/v1/events":
			w.Write([]byte(eventsJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	now := time.Date(2026, 10, 11, 14, 0, 5, 0, time.UTC)
	f := newFleetdSrc(NewFleetdClient(srv.URL, writeToken(t)), func() time.Time { return now }, "karmax", []string{"agent-01"})
	ctx := context.Background()
	if err := f.poll(ctx); err != nil {
		t.Fatalf("poll returns the roster's error only: %v", err)
	}
	f.pollActivity(ctx)
	f.pollToday(ctx)

	src := f.source(now)
	if !src.OK {
		t.Fatalf("the roster answered, so fleetd is ok: %+v", src)
	}
	for _, want := range []string{"tasks: 404", "activity: 404", "today: 404", "last_task: 404"} {
		if !strings.Contains(src.Error, want) {
			t.Errorf("error %q lacks %q", src.Error, want)
		}
	}
	if strings.Contains(src.Error, "status") || strings.Contains(src.Error, "events") {
		t.Errorf("error %q names routes that worked", src.Error)
	}
	if f.rows["agent-01"].State != "working" || f.orch == nil || len(f.events) != 2 {
		t.Fatalf("what worked was blanked: %+v %v %d", f.rows, f.orch, len(f.events))
	}
	// and the missing route's value is still the zero, not an error in the data
	if len(f.tasks) != 0 {
		t.Fatalf("tasks = %+v", f.tasks)
	}
}
