package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// contractServer answers the TUI's routes from a Fake fleet and records each
// request as "METHOD path?query body".
type contractServer struct {
	mu   sync.Mutex
	reqs []string
	fake *fleetapi.Fake
}

func (s *contractServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	line := r.Method + " " + r.URL.RequestURI()
	if len(body) > 0 {
		line += " " + string(body)
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, line)
	s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, `{"error":"no token"}`, http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	var out any
	switch {
	case r.URL.Path == fleetapi.PathWatch:
		out, _ = s.fake.Watch(ctx, 0)
	case r.URL.Path == fleetapi.PathScreens:
		sc, _ := s.fake.Screens(ctx, strings.Split(r.URL.Query().Get("workers"), ","), 3)
		out = fleetapi.ScreensReply{Screens: sc}
	case r.URL.Path == "/v1/tasks/20261025-access-3c4d":
		out, _ = s.fake.Task(ctx, "20261025-access-3c4d")
	case r.URL.Path == "/v1/run":
		out = fleetapi.RunReply{Task: "20261025-fix-it-ab12", Agent: "agent-02", Why: "free"}
	case r.URL.Path == fleetapi.PathAccess:
		out, _ = s.fake.Access(ctx)
	case r.URL.Path == "/v1/tasks/gone":
		w.WriteHeader(http.StatusNotFound)
		out = map[string]string{"error": "no task gone"}
	default:
		out = map[string]bool{"ok": true}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func TestTheHTTPClientSpeaksTheContract(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 10, 25, 21, 4, 0, 0, time.UTC) }
	s := &contractServer{fake: fleetapi.NewFake(fleetapi.RoleAdmin, now)}
	srv := httptest.NewServer(s)
	defer srv.Close()
	a := &apiClient{c: &client{base: srv.URL, token: "tok"}, from: "operator"}
	ctx := context.Background()

	snap, err := a.Watch(ctx, 7)
	if err != nil || len(snap.Workers) != 20 || snap.Me.Role != fleetapi.RoleAdmin {
		t.Fatalf("watch: %v, %+v", err, snap)
	}
	sc, err := a.Screens(ctx, []string{"agent-01", "agent-02:2"}, 40)
	if err != nil || len(sc) != 2 || sc[1].Worker != "agent-02:2" {
		t.Errorf("screens: %v %+v", err, sc)
	}
	d, err := a.Task(ctx, "20261025-access-3c4d")
	if err != nil || d.State != "open" || len(d.Reports) != 2 {
		t.Errorf("task: %v %+v", err, d)
	}
	if _, err := a.Task(ctx, "gone"); err == nil || err.Error() != "no task gone" {
		t.Errorf("a missing task: %v", err)
	}
	rep, err := a.Run(ctx, fleetapi.RunRequest{Brief: "fix it", Repo: "KARMAX", Agent: "agent-02", Model: "sonnet"})
	if err != nil || rep.Task != "20261025-fix-it-ab12" || rep.Agent != "agent-02" {
		t.Errorf("run: %v %+v", err, rep)
	}
	for _, err := range []error{
		a.Followup(ctx, "T1", "also this"),
		a.Answer(ctx, "T1", "yes"),
		a.Cancel(ctx, "T1", "not needed"),
		a.Done(ctx, "T1"),
		a.SetModel(ctx, "agent-03:2", "opus"),
	} {
		if err != nil {
			t.Error(err)
		}
	}
	if l, err := a.Access(ctx); err != nil || len(l.Devices) != 2 {
		t.Errorf("access: %v %+v", err, l)
	}
	if _, err := a.ChangeAccess(ctx, fleetapi.AccessChange{Op: fleetapi.AccessLend, Device: "friend-laptop", Agents: []string{"agent-10"}}); err != nil {
		t.Error(err)
	}

	want := []string{
		"GET /v1/watch?since=7",
		"GET /v1/screens?lines=40&workers=agent-01%2Cagent-02%3A2",
		"GET /v1/tasks/20261025-access-3c4d",
		"GET /v1/tasks/gone",
		`POST /v1/run {"agent":"agent-02","brief":"fix it","from":"operator","model":"sonnet","repo":"KARMAX","task":"`,
		`POST /v1/tasks/T1/followup {"from":"operator","text":"also this"}`,
		`POST /v1/tasks/T1/answer {"from":"operator","text":"yes"}`,
		`POST /v1/tasks/T1/cancel {"from":"operator","reason":"not needed"}`,
		`POST /v1/done {"agent":"","branch":"","keep_session":false,"outcome":"done","pr":"","task":"T1"}`,
		`POST /v1/agents/agent-03:2/model {"model":"opus","reset":false}`,
		"GET /v1/access",
		`POST /v1/access {"op":"lend","device":"friend-laptop","agents":["agent-10"]}`,
	}
	if len(s.reqs) != len(want) {
		t.Fatalf("requests:\n%s", strings.Join(s.reqs, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(s.reqs[i], w) {
			t.Errorf("request %d = %s\nwant %s", i, s.reqs[i], w)
		}
	}
}

func TestTheWatchSaysWhenFleetdIsTooOld(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	a := &apiClient{c: &client{base: srv.URL}, from: "operator"}
	if _, err := a.Watch(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "older than this fleetctl") {
		t.Errorf("err = %v", err)
	}
}

func TestTheWatchStopsWithItsContext(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	a := &apiClient{c: &client{base: srv.URL}, from: "operator"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { _, err := a.Watch(ctx, 0); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled watch returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not stop with its context")
	}
}

// fleetd that answers slowly was reached: saying "unreachable" sends a
// person to check the tailnet when the host is only busy.
func TestASlowAnswerIsNotCalledUnreachable(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	a := &apiClient{c: &client{base: srv.URL}, from: "operator"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := a.Screens(ctx, []string{"agent-01"}, 10)
	if err == nil || strings.Contains(err.Error(), "unreachable") || !strings.Contains(err.Error(), "did not answer in time") {
		t.Errorf("err = %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v: want it to be a deadline", err)
	}
}

func TestTUIFlags(t *testing.T) {
	if err := tuiCmd([]string{"--guest"}); err == nil || !strings.Contains(err.Error(), "--demo") {
		t.Errorf("--guest alone: %v", err)
	}
	if err := tuiCmd([]string{"extra"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("an argument: %v", err)
	}
	if err := tuiCmd([]string{"--demo"}); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("no terminal: %v", err)
	}
}
