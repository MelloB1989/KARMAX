package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"go.uber.org/zap"
)

// This file is the security surface for the browser-scoped token: a caller
// authenticated with it may run the browser tool and nothing else. The
// agent these tests build never has Start() called on it, so it holds no
// real tools (agent.Agent.ToolManifests/ExecuteTool need initModels, which
// only Start runs) — deliberately: these tests are about the GATE in
// handleCallTool/handleListTools, not about any particular tool's own
// behaviour, and a call that gets PAST the gate lands on agent.Agent's own
// "unknown tool" error, which is a perfectly good, deterministic stand-in
// for "reached execution" without the cost/flakiness risk of a fully
// started, model-backed agent.

// newScopeTestServer builds a Server with one agent ("a") registered but
// never started, and both a full and a browser-scoped token configured.
func newScopeTestServer(t *testing.T, fullToken, browserToken string) *Server {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "scope.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	reg := agent.NewRegistry(nil, db, zap.NewNop())
	if _, err := reg.Register(agent.AgentDef{ID: "a"}, nil, nil, nil); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	return New("127.0.0.1:0", 0, fullToken, browserToken, reg, db, nil, nil, &config.KarmaxConfig{}, zap.NewNop())
}

// withExecuteAgentToolSpy substitutes the package-level executeAgentTool var
// with a counting wrapper for the duration of one test, and restores it
// afterwards. It is the direct "was ExecuteTool invoked" observation the
// scope gate is required to prevent for a denied request.
func withExecuteAgentToolSpy(t *testing.T) *int {
	t.Helper()
	calls := 0
	orig := executeAgentTool
	executeAgentTool = func(ctx context.Context, ag *agent.Agent, name string, input map[string]any) (tools.ToolResult, error) {
		calls++
		return orig(ctx, ag, name, input)
	}
	t.Cleanup(func() { executeAgentTool = orig })
	return &calls
}

func callTool(srv *Server, token, name string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/tools/"+name, nil)
	r.SetPathValue("name", name)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.auth(srv.handleCallTool)(w, r)
	return w
}

func listTools(srv *Server, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.auth(srv.handleListTools)(w, r)
	return w
}

// The scoped token's one job: the browser tool goes through.
func TestScopedTokenMayCallTheBrowserTool(t *testing.T) {
	srv := newScopeTestServer(t, "full-tok", "browser-tok")
	calls := withExecuteAgentToolSpy(t)

	w := callTool(srv, "browser-tok", "browser")

	if w.Code == http.StatusForbidden {
		t.Fatalf("browser tool with the scoped token was forbidden: %d %s", w.Code, w.Body.String())
	}
	if *calls != 1 {
		t.Fatalf("ExecuteTool calls = %d, want 1 — a permitted tool must reach execution", *calls)
	}
}

// The security property this whole change exists for: a scoped token cannot
// reach a dangerous tool, and — critically — never even gets as far as
// asking the agent to run it.
func TestScopedTokenIsForbiddenFromADangerousTool(t *testing.T) {
	srv := newScopeTestServer(t, "full-tok", "browser-tok")
	calls := withExecuteAgentToolSpy(t)

	w := callTool(srv, "browser-tok", "shell.exec")

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if *calls != 0 {
		t.Fatalf("ExecuteTool calls = %d, want 0 — a scope-denied request must never reach tool execution", *calls)
	}
}

// A connector tool (the exact abuse vector the task describes — sending
// messages through the user's own accounts) is just as forbidden as
// shell.exec.
func TestScopedTokenIsForbiddenFromAConnectorTool(t *testing.T) {
	srv := newScopeTestServer(t, "full-tok", "browser-tok")
	calls := withExecuteAgentToolSpy(t)

	w := callTool(srv, "browser-tok", "email.send")

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if *calls != 0 {
		t.Fatalf("ExecuteTool calls = %d, want 0", *calls)
	}
}

// The full token is unrestricted, exactly as before this change.
func TestFullTokenMayCallEitherTool(t *testing.T) {
	srv := newScopeTestServer(t, "full-tok", "browser-tok")
	calls := withExecuteAgentToolSpy(t)

	for _, name := range []string{"browser", "shell.exec"} {
		w := callTool(srv, "full-tok", name)
		if w.Code == http.StatusForbidden {
			t.Fatalf("%s with the full token was forbidden: %d %s", name, w.Code, w.Body.String())
		}
	}
	if *calls != 2 {
		t.Fatalf("ExecuteTool calls = %d, want 2", *calls)
	}
}

// An unrecognized bearer token is still just unauthorized — introducing the
// scoped token must not change that.
func TestAnUnrecognizedTokenIsUnauthorized(t *testing.T) {
	srv := newScopeTestServer(t, "full-tok", "browser-tok")
	w := callTool(srv, "not-a-real-token", "browser")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
	}
}

// Auth disabled entirely (empty full token, development only) stays
// unrestricted — the scoped token must not narrow that existing behaviour.
func TestEmptyFullTokenDisablesAuthAndGrantsFullScope(t *testing.T) {
	srv := newScopeTestServer(t, "", "browser-tok")
	calls := withExecuteAgentToolSpy(t)

	w := callTool(srv, "", "shell.exec")

	if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Fatalf("dev mode (empty full token) should be unrestricted: %d %s", w.Code, w.Body.String())
	}
	if *calls != 1 {
		t.Fatalf("ExecuteTool calls = %d, want 1", *calls)
	}
}

// handleListTools must not 500/panic under either token — the real listing
// behaviour (only "browser" survives for a scoped caller) is covered by
// TestFilterManifestsForScopeKeepsOnlyBrowserForScopedCallers below, which
// exercises the exact function this handler calls.
func TestHandleListToolsIsReachableUnderBothScopes(t *testing.T) {
	srv := newScopeTestServer(t, "full-tok", "browser-tok")
	for _, tok := range []string{"full-tok", "browser-tok"} {
		w := listTools(srv, tok)
		if w.Code != http.StatusOK {
			t.Fatalf("token %q: status = %d, want 200: %s", tok, w.Code, w.Body.String())
		}
	}
}

func TestFilterManifestsForScopeKeepsOnlyBrowserForScopedCallers(t *testing.T) {
	manifests := []tools.ToolManifest{{Name: "browser"}, {Name: "shell.exec"}, {Name: "email.send"}}

	got := filterManifestsForScope(scopeBrowserOnly, manifests)
	if len(got) != 1 || got[0].Name != "browser" {
		t.Fatalf("scoped filter = %+v, want only browser", got)
	}

	got = filterManifestsForScope(scopeFull, manifests)
	if len(got) != 3 {
		t.Fatalf("full-scope filter = %+v, want all 3 manifests unfiltered", got)
	}
}

func TestScopeAllowsToolMatchesTheAllowlistExactly(t *testing.T) {
	cases := []struct {
		scope toolScope
		name  string
		want  bool
	}{
		{scopeBrowserOnly, "browser", true},
		{scopeBrowserOnly, "shell.exec", false},
		{scopeBrowserOnly, "email.send", false},
		{scopeBrowserOnly, "whatsapp.send_message", false},
		{scopeBrowserOnly, "harness.send", false},
		{scopeFull, "shell.exec", true},
		{scopeFull, "anything.at.all", true},
	}
	for _, c := range cases {
		if got := scopeAllowsTool(c.scope, c.name); got != c.want {
			t.Errorf("scopeAllowsTool(%v, %q) = %v, want %v", c.scope, c.name, got, c.want)
		}
	}
}

// scopeFromContext must deny by default: a context nobody tagged is
// browser-only, never full.
func TestScopeFromContextDefaultsToBrowserOnly(t *testing.T) {
	if got := scopeFromContext(context.Background()); got != scopeBrowserOnly {
		t.Fatalf("scopeFromContext(untagged) = %v, want scopeBrowserOnly", got)
	}
}
