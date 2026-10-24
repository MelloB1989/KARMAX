package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/fleet/internal/identity"
)

// asAgent makes r an agent's: the key its container's env file holds, made
// with the tests' full token.
func asAgent(r *http.Request, name string) *http.Request {
	r.Header.Set(identity.Header, identity.Value(name, identity.Key("full", identity.Base(name))))
	return r
}

// ado is do as an agent, with the relay token and its key.
func ado(t *testing.T, h http.Handler, method, path, agent, body string) *http.Response {
	t.Helper()
	req := asAgent(newReq(method, path, "relay", body), agent)
	return serve(h, req)
}

func TestAnAgentKeyThatDoesNotVerifyIsRefused(t *testing.T) {
	_, h := setup(t)
	for _, v := range []string{
		identity.Value("agent-03", identity.Key("relay", "agent-03")), // made without the full token
		identity.Value("agent-05", identity.Key("full", "agent-03")),  // another agent's
		"agent-03",
	} {
		req := newReq("GET", "/v1/status", "relay", "")
		req.Header.Set(identity.Header, v)
		if resp := serve(h, req); resp.StatusCode != 403 {
			t.Errorf("%q: %d, want 403", v, resp.StatusCode)
		}
	}
	if resp := ado(t, h, "GET", "/v1/status", "agent-03", ""); resp.StatusCode != 200 {
		t.Fatalf("a good key: %d", resp.StatusCode)
	}
}

func TestVerifiedAgentNamesTheWorkerWhoseContainerKeyItHolds(t *testing.T) {
	var got []string
	tok := Tokens{Full: "full", Relay: "relay"}
	h := withAgent(tok, (&fake{}).IsWorker, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := verifiedAgent(r)
		got = append(got, name+map[bool]string{true: " ok", false: " no"}[ok])
	}))
	serve(h, asAgent(newReq("GET", "/", "relay", ""), "agent-03:2"))
	serve(h, newReq("GET", "/", "relay", ""))
	if strings.Join(got, "|") != "agent-03:2 ok| no" {
		t.Fatalf("verifiedAgent = %q", got)
	}
}

// With its key, an agent speaks as itself and as nobody else.
func TestAVerifiedAgentTellsAsItself(t *testing.T) {
	f, h := setup(t)
	if resp := ado(t, h, "POST", "/v1/tell", "agent-05", `{"to":"agent-03","text":"hi"}`); resp.StatusCode != 200 {
		t.Fatalf("tell without from: %d", resp.StatusCode)
	}
	if resp := ado(t, h, "POST", "/v1/tell", "agent-05", `{"from":"agent-05","to":"operator","text":"ready"}`); resp.StatusCode != 200 {
		t.Fatalf("tell as itself: %d", resp.StatusCode)
	}
	if resp := ado(t, h, "POST", "/v1/tell", "agent-05", `{"from":"agent-03","to":"operator","text":"forged"}`); resp.StatusCode != 403 {
		t.Fatalf("tell as another agent: %d", resp.StatusCode)
	}
	if got := strings.Join(f.calls, "|"); got != "tell agent-05 agent-03 hi|tell agent-05 operator ready" {
		t.Fatalf("calls = %s", got)
	}
}

func newReq(method, path, token, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func serve(h http.Handler, req *http.Request) *http.Response {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

// A container's key verifies any name of its agent's; only a worker the
// config has is accepted, wherever the verified name is used.
func TestAVerifiedNameMustBeAWorkerTheConfigHas(t *testing.T) {
	_, h := setup(t)
	if resp := ado(t, h, "GET", "/v1/status", "agent-03:2", ""); resp.StatusCode != 200 {
		t.Fatalf("slot 2 of a two-slot agent: %d", resp.StatusCode)
	}
	for _, name := range []string{"agent-03:9", "agent-03-s2", "agent-03:1"} {
		if resp := ado(t, h, "GET", "/v1/status", name, ""); resp.StatusCode != 403 {
			t.Errorf("%s, with agent-03's key: %d, want 403", name, resp.StatusCode)
		}
	}
}
