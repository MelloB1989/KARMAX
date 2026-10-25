package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/identity"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// sandboxFleet is the fake with two sandboxes: agent-09 works on T-guest for
// friend@example.com, and agent-10 on T-other for someone else.
type sandboxFleet struct{ *fake }

func (f sandboxFleet) Status(now time.Time) []daemon.Row {
	return append(f.fake.Status(now),
		daemon.Row{Agent: "agent-09", State: "working", Task: "T-guest"},
		daemon.Row{Agent: "agent-10", State: "working", Task: "T-other"},
		daemon.Row{Agent: "agent-09:2", State: "standby"})
}

func (f sandboxFleet) TaskAgent(task string) (*ledger.Task, error) {
	switch task {
	case "T-guest":
		return &ledger.Task{ID: task, Agent: "agent-09", Assigner: "friend@example.com"}, nil
	case "T-other":
		return &ledger.Task{ID: task, Agent: "agent-10", Assigner: "other@example.com"}, nil
	}
	return f.fake.TaskAgent(task)
}

func sandboxSetup(t *testing.T) (*fake, http.Handler) {
	t.Helper()
	f := &fake{}
	return f, Handler(sandboxFleet{f}, Tokens{Full: "full", Relay: "relay", Console: "console", Sandboxes: []string{"agent-09", "agent-10"}}, nil)
}

// asSandbox is a request with a sandbox's key, and the worker it says it is
// when worker is set (a slot), as a sandbox's fleetctl sends it.
func asSandbox(t *testing.T, h http.Handler, method, path, key, worker, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	if worker != "" {
		req.Header.Set(identity.Header, identity.Value(worker, key))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The sandbox key reaches exactly its allowed routes: every other route in
// the index refuses it, so a route added later is closed to it by default.
func TestASandboxKeyIsOnlyItsOwnScope(t *testing.T) {
	_, h := sandboxSetup(t)
	var index []Route
	if err := json.Unmarshal(do(t, h, "GET", "/v1", "full", "").Body.Bytes(), &index); err != nil || len(index) < 20 {
		t.Fatalf("index: %v (%d routes)", err, len(index))
	}
	key := identity.SandboxKey("full", "agent-09")
	allowed := map[string]struct{ path, body string }{
		"GET /v1/whoami":     {"/v1/whoami", ""},
		"POST /v1/tell":      {"/v1/tell", `{"to":"friend@example.com","text":"done"}`},
		"POST /v1/ask":       {"/v1/ask", `{"text":"which file?"}`},
		"GET /v1/tasks/{id}": {"/v1/tasks/T-guest", ""},
		"GET /v1/wait":       {"/v1/wait?task=T-guest&timeout=1s", ""},
	}
	seen := 0
	for _, r := range index {
		method, pattern, _ := strings.Cut(r.Route, " ")
		if a, ok := allowed[r.Route]; ok {
			seen++
			if rec := asSandbox(t, h, method, a.path, key, "", a.body); rec.Code != 200 {
				t.Errorf("%s with a sandbox key: %d %s, want 200", r.Route, rec.Code, rec.Body)
			}
			continue
		}
		path := strings.NewReplacer("{id...}", "a/b", "{id}", "T-guest", "{agent}", "agent-09", "{name}", "x").Replace(pattern)
		if rec := asSandbox(t, h, method, path, key, "", "{}"); rec.Code != 403 {
			t.Errorf("%s with a sandbox key: %d, want 403", r.Route, rec.Code)
		}
	}
	if seen != len(allowed) {
		t.Errorf("the index lists %d of the sandbox's %d routes", seen, len(allowed))
	}
	// Telemetry is the receiver's (otlp), and memory, the gateway and secrets
	// are not here at all for it.
	for _, p := range []string{"/v1/memory/recall?q=x", "/v1/karmax/tools", "/v1/secrets"} {
		if rec := asSandbox(t, h, "GET", p, key, "", ""); rec.Code != 403 {
			t.Errorf("GET %s with a sandbox key: %d, want 403", p, rec.Code)
		}
	}
}

func TestASandboxIsItsAgentToWhoami(t *testing.T) {
	_, h := sandboxSetup(t)
	key := identity.SandboxKey("full", "agent-09")
	for worker, want := range map[string]string{"": "agent-09", "agent-09:2": "agent-09:2"} {
		rec := asSandbox(t, h, "GET", "/v1/whoami", key, worker, "")
		var who map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &who)
		if rec.Code != 200 || who["scope"] != "sandbox" || who["name"] != want {
			t.Errorf("whoami as %q = %d %v, want sandbox %s", worker, rec.Code, who, want)
		}
	}
}

// A sandbox speaks as itself, to the guest whose task it holds, and to no one
// else: not the operator, the orchestrator, another agent or another guest.
func TestASandboxTellsOnlyItsAssigner(t *testing.T) {
	f, h := sandboxSetup(t)
	key := identity.SandboxKey("full", "agent-09")
	cases := []struct {
		worker, body string
		want         int
	}{
		{"", `{"to":"friend@example.com","text":"done"}`, 200},
		{"", `{"to":"friend@example.com","text":"half way","kind":"progress"}`, 200},
		{"", `{"from":"agent-09","to":"friend@example.com","text":"done"}`, 200},
		{"", `{"from":"agent-03","to":"friend@example.com","text":"done"}`, 403},
		{"", `{"from":"operator","to":"friend@example.com","text":"done"}`, 403},
		{"", `{"to":"operator","text":"hello"}`, 403},
		{"", `{"to":"karmax","text":"hello"}`, 403},
		{"", `{"to":"agent-03","text":"hello"}`, 403},
		{"", `{"to":"other@example.com","text":"hello"}`, 403},
		{"", `{"to":"friend@example.com/laptop","text":"hello"}`, 403},
		// Its second slot holds no task: it has no one to tell.
		{"agent-09:2", `{"to":"friend@example.com","text":"hello"}`, 403},
	}
	for _, c := range cases {
		if rec := asSandbox(t, h, "POST", "/v1/tell", key, c.worker, c.body); rec.Code != c.want {
			t.Errorf("tell %s as %q: %d %s, want %d", c.body, c.worker, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
	}
	for _, call := range f.calls {
		if !strings.HasPrefix(call, "tell agent-09 friend@example.com ") {
			t.Errorf("a refused tell went out: %s", call)
		}
	}
	if len(f.calls) != 3 {
		t.Errorf("calls = %v", f.calls)
	}
}

// A key only fleetd's full token makes, for a sandbox fleet.yaml has: not an
// ordinary agent's, not another token's, and never another sandbox's slot.
func TestASandboxKeyOfAnotherAgentIsRefused(t *testing.T) {
	f, h := sandboxSetup(t)
	nine := identity.SandboxKey("full", "agent-09")
	cases := []struct {
		name, key, worker, method, path string
		want                            int
	}{
		{"an ordinary agent's sandbox key", identity.SandboxKey("full", "agent-03"), "", "GET", "/v1/whoami", 401},
		{"another full token's", identity.SandboxKey("other", "agent-09"), "", "GET", "/v1/whoami", 401},
		{"an agent key as a bearer", identity.Key("full", "agent-09"), "", "GET", "/v1/whoami", 401},
		{"another sandbox's name", nine, "agent-10", "GET", "/v1/whoami", 403},
		{"a slot it does not have", nine, "agent-09:5", "GET", "/v1/whoami", 403},
		{"another sandbox's task", nine, "", "GET", "/v1/tasks/T-other", 403},
		{"the operator's task", nine, "", "GET", "/v1/tasks/T1", 403},
		{"a wait on another's task", nine, "", "GET", "/v1/wait?task=T-other&timeout=1s", 403},
		{"a wait by agent", nine, "", "GET", "/v1/wait?agent=agent-03&timeout=1s", 403},
	}
	for _, c := range cases {
		if rec := asSandbox(t, h, c.method, c.path, c.key, c.worker, ""); rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
	}
	// The key in the identity header must be the bearer's own.
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+nine)
	req.Header.Set(identity.Header, identity.Value("agent-09", identity.SandboxKey("full", "agent-10")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("a header with another sandbox's key: %d, want 403", rec.Code)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %v", f.calls)
	}
}

// What a sandbox reads and waits on is its own task, as itself.
func TestASandboxWaitsOnItsOwnTaskAsItself(t *testing.T) {
	f, h := sandboxSetup(t)
	key := identity.SandboxKey("full", "agent-09")
	for _, p := range []string{"/v1/wait?task=T-guest&agent=agent-03&timeout=1s", "/v1/wait?task=T-guest&new=friend@example.com&timeout=1s"} {
		if rec := asSandbox(t, h, "GET", p, key, "", ""); rec.Code != 200 {
			t.Errorf("GET %s: %d %s", p, rec.Code, rec.Body)
		}
	}
	want := []string{"wait  T-guest 0", "waitnew T-guest agent-09"}
	if fmt.Sprint(f.calls) != fmt.Sprint(want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
}
