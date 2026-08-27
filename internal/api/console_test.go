package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/connectors"
	githubconn "github.com/MelloB1989/karmax/internal/connectors/github"
	googleconn "github.com/MelloB1989/karmax/internal/connectors/google"
	slackconn "github.com/MelloB1989/karmax/internal/connectors/slack"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func consoleTestServer(t *testing.T) (*ConsoleServer, *store.Store) {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "c.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	srv := NewConsole("127.0.0.1:0", "", ConsoleDeps{
		Store: db, Config: &config.KarmaxConfig{}, Log: zap.NewNop(),
	})
	return srv, db
}

func do(t *testing.T, srv *ConsoleServer, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		raw, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(w, r)
	return w
}

func bootstrapAdmin(t *testing.T, srv *ConsoleServer) string {
	t.Helper()
	w := do(t, srv, "POST", "/api/console/auth/bootstrap", "",
		map[string]string{"name": "Nikhil", "member": "nikhil", "password": "correct-horse"})
	if w.Code != http.StatusOK {
		t.Fatalf("bootstrap failed: %d %s", w.Code, w.Body.String())
	}
	var s sessionResponse
	json.Unmarshal(w.Body.Bytes(), &s)
	return s.Token
}

func TestEveryDataRouteRequiresASession(t *testing.T) {
	srv, _ := consoleTestServer(t)

	// Nothing that reads org data may answer an unauthenticated caller.
	for _, path := range []string{
		"/api/console/cases", "/api/console/agents", "/api/console/recipes",
		"/api/console/approvals", "/api/console/audit", "/api/console/connectors",
		"/api/console/settings", "/api/console/auth/me",
	} {
		if w := do(t, srv, "GET", path, "", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s answered %d without a token, want 401", path, w.Code)
		}
	}
}

func TestAForgedTokenIsRejected(t *testing.T) {
	srv, _ := consoleTestServer(t)
	bootstrapAdmin(t, srv)

	if w := do(t, srv, "GET", "/api/console/cases", "not-a-real-token", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a made-up token got %d, want 401", w.Code)
	}
}

func TestBootstrapOnlyWorksOnce(t *testing.T) {
	srv, _ := consoleTestServer(t)

	w := do(t, srv, "GET", "/api/console/auth/bootstrap-status", "", nil)
	var st struct {
		Needs bool `json:"needs_bootstrap"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	if !st.Needs {
		t.Fatal("a fresh install should need bootstrapping")
	}

	bootstrapAdmin(t, srv)

	// The second attempt must be refused server-side, whatever the client shows.
	w = do(t, srv, "POST", "/api/console/auth/bootstrap", "",
		map[string]string{"name": "Attacker", "member": "attacker", "password": "hunter2222"})
	if w.Code != http.StatusConflict {
		t.Errorf("a second bootstrap got %d, want 409 — this would be account takeover", w.Code)
	}

	w = do(t, srv, "GET", "/api/console/auth/bootstrap-status", "", nil)
	json.Unmarshal(w.Body.Bytes(), &st)
	if st.Needs {
		t.Error("the install still reports needing bootstrap after an admin exists")
	}
}

func TestLogoutRevokesTheTokenImmediately(t *testing.T) {
	srv, _ := consoleTestServer(t)
	token := bootstrapAdmin(t, srv)

	if w := do(t, srv, "GET", "/api/console/auth/me", token, nil); w.Code != http.StatusOK {
		t.Fatalf("session did not work before logout: %d", w.Code)
	}
	if w := do(t, srv, "POST", "/api/console/auth/logout", token, nil); w.Code != http.StatusOK {
		t.Fatalf("logout failed: %d", w.Code)
	}
	if w := do(t, srv, "GET", "/api/console/auth/me", token, nil); w.Code != http.StatusUnauthorized {
		t.Error("the token still worked after logout")
	}
}

// A viewer can read. A viewer must not be able to approve an action, enable a
// workflow, or change a credential.
func TestAViewerCannotWrite(t *testing.T) {
	srv, db := consoleTestServer(t)
	bootstrapAdmin(t, srv)

	if _, err := db.CreateConsoleUser("reader", "Reader", "viewer", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	w := do(t, srv, "POST", "/api/console/auth/login", "",
		map[string]string{"member": "reader", "password": "correct-horse"})
	var sess sessionResponse
	json.Unmarshal(w.Body.Bytes(), &sess)
	if sess.Token == "" {
		t.Fatalf("viewer could not log in: %s", w.Body.String())
	}

	// Reading is allowed.
	if got := do(t, srv, "GET", "/api/console/audit", sess.Token, nil); got.Code != http.StatusOK {
		t.Errorf("a viewer could not read the audit log: %d", got.Code)
	}

	writes := []struct{ method, path string }{
		{"POST", "/api/console/recipes/anything/enable"},
		{"POST", "/api/console/approvals/x/decision"},
		{"POST", "/api/console/connectors/slack/credentials"},
		{"PUT", "/api/console/settings/roles/someone"},
		{"POST", "/api/console/settings/directory/sync"},
	}
	for _, wr := range writes {
		got := do(t, srv, wr.method, wr.path, sess.Token, map[string]string{"role": "admin"})
		if got.Code != http.StatusForbidden {
			t.Errorf("%s %s answered %d for a viewer, want 403", wr.method, wr.path, got.Code)
		}
	}
}

// An operator may act, but must not be able to hand out roles or credentials.
func TestAnOperatorCannotGrantRoles(t *testing.T) {
	srv, db := consoleTestServer(t)
	bootstrapAdmin(t, srv)
	if _, err := db.CreateConsoleUser("ops", "Ops", "operator", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	w := do(t, srv, "POST", "/api/console/auth/login", "",
		map[string]string{"member": "ops", "password": "correct-horse"})
	var sess sessionResponse
	json.Unmarshal(w.Body.Bytes(), &sess)

	if got := do(t, srv, "PUT", "/api/console/settings/roles/reader", sess.Token,
		map[string]string{"role": "admin"}); got.Code != http.StatusForbidden {
		t.Errorf("an operator could change a role (%d) — that is privilege escalation", got.Code)
	}
}

// The last admin must not be able to lock everyone out of role management.
func TestTheOnlyAdminCannotDemoteThemselves(t *testing.T) {
	srv, _ := consoleTestServer(t)
	token := bootstrapAdmin(t, srv)

	got := do(t, srv, "PUT", "/api/console/settings/roles/nikhil", token, map[string]string{"role": "viewer"})
	if got.Code != http.StatusConflict {
		t.Errorf("the only admin demoted themselves (%d) — the console would have no admin left", got.Code)
	}
}

func TestRecipeNamesCannotEscapeTheRecipeDirectory(t *testing.T) {
	for _, bad := range []string{"../../etc/passwd", "a/b", "..", "", "Upper", "with space", strings.Repeat("x", 100)} {
		if _, err := recipePath(bad); err == nil {
			t.Errorf("%q was accepted as a recipe name", bad)
		}
	}
	if _, err := recipePath("stale-review-nudge"); err != nil {
		t.Errorf("a normal name was rejected: %v", err)
	}
}

// A recipe must never be born live, whatever the YAML says.
func TestASavedRecipeIsAlwaysDisabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KARMAX_RECIPES_DIR", dir)

	srv, _ := consoleTestServer(t)
	token := bootstrapAdmin(t, srv)

	yaml := "name: nightly\nenabled: true\non:\n  schedule: \"0 30 8 * * *\"\nsteps:\n  - log: hello\n"
	w := do(t, srv, "POST", "/api/console/recipes", token,
		map[string]string{"name": "nightly", "yaml": yaml})
	if w.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}

	saved, err := os.ReadFile(filepath.Join(dir, "nightly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "enabled: true") {
		t.Error("a recipe was saved enabled — generating a workflow must never start it firing")
	}

	var detail recipeDetail
	json.Unmarshal(w.Body.Bytes(), &detail)
	if detail.Enabled {
		t.Error("the response claims the new recipe is enabled")
	}
}

func TestEnablingARecipeIsASeparateExplicitCall(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KARMAX_RECIPES_DIR", dir)

	srv, _ := consoleTestServer(t)
	token := bootstrapAdmin(t, srv)
	yaml := "name: nightly\non:\n  schedule: \"0 30 8 * * *\"\nsteps:\n  - log: hello\n"
	if w := do(t, srv, "POST", "/api/console/recipes", token,
		map[string]string{"name": "nightly", "yaml": yaml}); w.Code != http.StatusOK {
		t.Fatalf("save failed: %s", w.Body.String())
	}

	w := do(t, srv, "POST", "/api/console/recipes/nightly/enable", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("enable failed: %d %s", w.Code, w.Body.String())
	}
	var detail recipeDetail
	json.Unmarshal(w.Body.Bytes(), &detail)
	if !detail.Enabled {
		t.Error("the recipe did not come back enabled")
	}

	// And an edit must not silently flip it back off.
	if w := do(t, srv, "PUT", "/api/console/recipes/nightly", token,
		map[string]string{"yaml": "name: nightly\non:\n  schedule: \"0 45 9 * * *\"\nsteps:\n  - log: hi\n"}); w.Code != http.StatusOK {
		t.Fatalf("edit failed: %s", w.Body.String())
	}
	w = do(t, srv, "GET", "/api/console/recipes/nightly", token, nil)
	json.Unmarshal(w.Body.Bytes(), &detail)
	if !detail.Enabled {
		t.Error("editing a live recipe silently disabled it")
	}
}

func TestListsSerialiseAsArraysWhenEmpty(t *testing.T) {
	t.Setenv("KARMAX_RECIPES_DIR", t.TempDir())
	srv, _ := consoleTestServer(t)
	token := bootstrapAdmin(t, srv)

	// A null here is a crash in the console, which maps over these directly.
	for path, key := range map[string]string{
		"/api/console/cases":      "cases",
		"/api/console/agents":     "agents",
		"/api/console/recipes":    "recipes",
		"/api/console/approvals":  "approvals",
		"/api/console/audit":      "events",
		"/api/console/connectors": "connectors",
	} {
		w := do(t, srv, "GET", path, token, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
			continue
		}
		var body map[string]json.RawMessage
		json.Unmarshal(w.Body.Bytes(), &body)
		if string(body[key]) == "null" {
			t.Errorf("%s returned null for %q instead of []", path, key)
		}
	}
}

// The console is published; the API port is not. No route here may run a
// command, and this test fails loudly if one is ever added.
//
// Asserted against the routing table rather than a status code: every unmatched
// path falls through to the SPA, so a 404 would prove nothing about whether a
// handler exists.
func TestTheConsoleExposesNoToolExecutionRoute(t *testing.T) {
	srv, _ := consoleTestServer(t)

	for _, path := range []string{
		"/api/tools/shell.exec", "/api/console/tools/shell.exec",
		"/api/chat", "/api/console/chat", "/api/console/exec",
	} {
		r := httptest.NewRequest("POST", path, nil)
		if _, pattern := srv.mux.Handler(r); pattern != "/" {
			t.Errorf("%s is routed to %q — the console must carry no tool execution", path, pattern)
		}
	}
}

// Every route the console DOES carry is one of the documented console routes.
// A stray registration is caught here rather than in production.
func TestEveryConsoleRouteIsUnderTheConsoleNamespace(t *testing.T) {
	srv, _ := consoleTestServer(t)

	for _, path := range []string{"/api/console/cases", "/api/console/settings"} {
		r := httptest.NewRequest("GET", path, nil)
		if _, pattern := srv.mux.Handler(r); pattern == "/" {
			t.Errorf("%s fell through to the SPA — the route is missing", path)
		}
	}
}

func TestSecretsAreNeverReturnedInFull(t *testing.T) {
	srv, _ := consoleTestServer(t)
	srv.cfg = &config.KarmaxConfig{}
	srv.cfg.AI.Providers = map[string]config.ProviderConfig{
		"azure_openai": {APIKey: "super-secret-key-abcd", BaseURL: "https://example.invalid"},
	}
	token := bootstrapAdmin(t, srv)

	w := do(t, srv, "GET", "/api/console/settings", token, nil)
	if strings.Contains(w.Body.String(), "super-secret-key-abcd") {
		t.Error("the settings endpoint returned an API key in full")
	}
	if !strings.Contains(w.Body.String(), "abcd") {
		t.Error("the settings endpoint should still show the last 4 so keys can be told apart")
	}
}

// A byte-sliced truncation splits a multi-byte character and emits invalid
// UTF-8, which then shows up as a corruption marker in the console.
func TestTruncateDoesNotSplitCharacters(t *testing.T) {
	long := strings.Repeat("é", 300)
	got := truncate(long, 240)

	if !utf8.ValidString(got) {
		t.Error("truncation produced invalid UTF-8")
	}
	if runes := []rune(got); len(runes) != 241 { // 240 + the ellipsis
		t.Errorf("expected 240 characters plus an ellipsis, got %d runes", len(runes))
	}
	if short := truncate("fine", 240); short != "fine" {
		t.Errorf("a short string was altered: %q", short)
	}
}

// A connector configured outside the credential store must not be reported as
// "not configured": a status display that contradicts what the operator can
// plainly see trains them to ignore it.
func TestAConnectorConfiguredElsewhereIsNotReportedMissing(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-from-env")

	srv, _ := consoleTestServer(t)
	srv.conns = connectors.NewHost(nil, nil, nil, zap.NewNop())
	srv.conns.Register(slackconn.New())

	got := srv.summariseConnector(slackconn.New().Manifest())
	if got.Status == "not_configured" {
		t.Error("Slack reported not_configured while its token was present in the environment")
	}
	if !strings.Contains(got.Detail, "outside the console") {
		t.Errorf("the detail does not explain where it came from: %q", got.Detail)
	}
}

// The member a flow binds to comes from the SESSION. If it came from the
// request body, one employee could bind their Google account to another's name.
func TestConnectStartRequiresASession(t *testing.T) {
	srv, _ := consoleTestServer(t)
	if w := do(t, srv, "POST", "/api/console/connectors/google/connect", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("connect started without a session: %d", w.Code)
	}
}

// The callback is opened by a browser with no bearer token, so the state token
// is the whole of its security.
func TestTheOAuthCallbackRejectsAnUnknownState(t *testing.T) {
	srv, _ := consoleTestServer(t)

	w := do(t, srv, "GET", "/api/console/oauth/google/callback?code=x&state=forged", "", nil)
	if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "Connected") {
		t.Fatal("a forged state produced a connection")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for an unknown state, got %d", w.Code)
	}
}

// A refusal is not a failure; showing a red error page for clicking Cancel is
// its own small bug.
func TestCancellingConsentIsNotAnError(t *testing.T) {
	srv, _ := consoleTestServer(t)

	w := do(t, srv, "GET", "/api/console/oauth/google/callback?error=access_denied", "", nil)
	if w.Code != http.StatusOK {
		t.Errorf("cancelling produced %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "cancelled") {
		t.Errorf("the page does not explain what happened: %s", w.Body.String())
	}
}

// Taking away access someone granted is an admin action; disconnecting yourself
// is not.
func TestDisconnectingSomeoneElseNeedsAdmin(t *testing.T) {
	srv, db := consoleTestServer(t)
	bootstrapAdmin(t, srv)
	if _, err := db.CreateConsoleUser("ops", "Ops", "operator", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	w := do(t, srv, "POST", "/api/console/auth/login", "",
		map[string]string{"member": "ops", "password": "correct-horse"})
	var sess sessionResponse
	json.Unmarshal(w.Body.Bytes(), &sess)

	if got := do(t, srv, "DELETE", "/api/console/connectors/google/connection?member=nikhil", sess.Token, nil); got.Code != http.StatusForbidden {
		t.Errorf("an operator disconnected someone else's account: %d", got.Code)
	}
	// Their own is fine.
	if got := do(t, srv, "DELETE", "/api/console/connectors/google/connection", sess.Token, nil); got.Code != http.StatusOK {
		t.Errorf("an operator could not disconnect themselves: %d", got.Code)
	}
}

// A list of names must not carry everybody's tokens.
func TestConnectionsListingCarriesNoTokens(t *testing.T) {
	srv, db := consoleTestServer(t)
	srv.conns = connectors.NewHost(nil, nil, nil, zap.NewNop())
	srv.conns.Register(googleconn.New())
	token := bootstrapAdmin(t, srv)
	if err := db.SaveUserCredential(store.UserCredential{
		Connector: "google", Member: "nikhil", Account: "n@acme.com",
		AccessToken: "at-secret", RefreshToken: "rt-secret",
	}); err != nil {
		t.Fatal(err)
	}

	w := do(t, srv, "GET", "/api/console/connectors/google/connections", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Error("the connections listing returned tokens")
	}
	if !strings.Contains(w.Body.String(), "n@acme.com") {
		t.Error("the listing omits the account, which is what the screen shows")
	}
}

// The console decides whether to show a "connect your account" panel from this
// call, so an install-wide connector must not answer with an empty list — that
// would render the panel on Slack, where it means nothing.
func TestConnectionsAreOnlyOfferedForPerUserConnectors(t *testing.T) {
	srv, _ := consoleTestServer(t)
	srv.conns = connectors.NewHost(nil, nil, nil, zap.NewNop())
	srv.conns.Register(slackconn.New())
	token := bootstrapAdmin(t, srv)

	if w := do(t, srv, "GET", "/api/console/connectors/slack/connections", token, nil); w.Code != http.StatusNotFound {
		t.Errorf("an install-wide connector offered per-person connections: %d", w.Code)
	}
	if w := do(t, srv, "POST", "/api/console/connectors/slack/connect", token, nil); w.Code != http.StatusBadRequest {
		t.Errorf("a per-person flow started for an install-wide connector: %d", w.Code)
	}
}

// A per-user connector cannot be connected until an admin has set up the org's
// OAuth app — there is nothing to authorise against.
func TestConnectRefusesBeforeTheOrgAppExists(t *testing.T) {
	srv, _ := consoleTestServer(t)
	srv.conns = connectors.NewHost(nil, nil, nil, zap.NewNop())
	srv.conns.Register(googleconn.New())
	token := bootstrapAdmin(t, srv)

	w := do(t, srv, "POST", "/api/console/connectors/google/connect", token, nil)
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 before the OAuth app is configured, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "admin has to set up") {
		t.Errorf("the error does not say what is missing: %s", w.Body.String())
	}
}

// The list said "run a health check to confirm" and the health check answered
// "no credentials saved yet" — a loop that could never reach healthy, about a
// bot that was visibly working. The check must ask the CONNECTOR, not just the
// credential store.
func TestHealthCheckAsksTheConnectorNotOnlyTheStore(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-from-env")
	t.Setenv("SLACK_APP_TOKEN", "")

	srv, _ := consoleTestServer(t)
	srv.conns = connectors.NewHost(nil, nil, nil, zap.NewNop())
	srv.conns.Register(slackconn.New())
	token := bootstrapAdmin(t, srv)

	w := do(t, srv, "POST", "/api/console/connectors/slack/health-check", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)

	// It may well fail (no network in a test, or a missing app token) — what it
	// must NOT do is claim nothing is configured.
	if res["status"] == "not_configured" {
		t.Errorf("the health check reported not_configured for a connector whose token is "+
			"present in the environment: %v", res["detail"])
	}
}

// Checking a per-user connector against the org's app config alone would report
// healthy for something nobody can actually use.
func TestPerUserHealthIsCheckedAsAPerson(t *testing.T) {
	srv, db := consoleTestServer(t)
	srv.conns = connectors.NewHost(nil, nil, nil, zap.NewNop())
	srv.conns.Register(googleconn.New())
	token := bootstrapAdmin(t, srv)

	if err := db.SaveCredential(store.Credential{
		Connector: "google", Config: map[string]string{"client_id": "cid", "client_secret": "s"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	w := do(t, srv, "POST", "/api/console/connectors/google/health-check", token, nil)
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)

	if res["status"] == "healthy" {
		t.Error("a per-user connector reported healthy with nobody connected")
	}
	if !strings.Contains(fmt.Sprint(res["detail"]), "not connected your own account") {
		t.Errorf("the detail does not say what is missing: %v", res["detail"])
	}
}

// Handing out console access hands out the ability to approve actions and read
// the org's memory. It is not an operator-level decision.
func TestUserManagementIsAdminOnly(t *testing.T) {
	srv, db := consoleTestServer(t)
	bootstrapAdmin(t, srv)
	if _, err := db.CreateConsoleUser("ops", "Ops", "operator", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	w := do(t, srv, "POST", "/api/console/auth/login", "",
		map[string]string{"member": "ops", "password": "correct-horse"})
	var sess sessionResponse
	json.Unmarshal(w.Body.Bytes(), &sess)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/console/users"},
		{"POST", "/api/console/users"},
		{"PUT", "/api/console/users/nikhil"},
		{"DELETE", "/api/console/users/nikhil"},
		{"PUT", "/api/console/organisation"},
	} {
		if got := do(t, srv, tc.method, tc.path, sess.Token, map[string]string{"role": "admin"}); got.Code != http.StatusForbidden {
			t.Errorf("%s %s answered %d for an operator, want 403", tc.method, tc.path, got.Code)
		}
	}

	// Reading the org is fine for anyone signed in — an agent's standing
	// context is not a secret from the people it works alongside.
	if got := do(t, srv, "GET", "/api/console/organisation", sess.Token, nil); got.Code != http.StatusOK {
		t.Errorf("an operator could not read the organisation: %d", got.Code)
	}
}

// An install with no admin has nobody who can appoint one, and the only way
// back is editing the database by hand.
func TestTheLastAdminCannotBeRemovedOrDemoted(t *testing.T) {
	srv, _ := consoleTestServer(t)
	token := bootstrapAdmin(t, srv)

	if w := do(t, srv, "DELETE", "/api/console/users/nikhil", token, nil); w.Code != http.StatusConflict {
		t.Errorf("the only admin was deleted: %d", w.Code)
	}
	if w := do(t, srv, "PUT", "/api/console/users/nikhil", token, map[string]string{"role": "viewer"}); w.Code != http.StatusConflict {
		t.Errorf("the only admin was demoted: %d", w.Code)
	}

	// With a second admin, both become allowed.
	if w := do(t, srv, "POST", "/api/console/users", token, map[string]string{
		"member": "second", "name": "Second", "role": "admin", "password": "correct-horse",
	}); w.Code != http.StatusOK {
		t.Fatalf("could not create a second admin: %s", w.Body.String())
	}
	if w := do(t, srv, "PUT", "/api/console/users/nikhil", token, map[string]string{"role": "viewer"}); w.Code != http.StatusOK {
		t.Errorf("demotion still refused with two admins: %d %s", w.Code, w.Body.String())
	}
}

// Changing your own password requires proving you know it, or a walk-up at an
// unlocked laptop takes the account.
func TestChangingYourOwnPasswordNeedsTheCurrentOne(t *testing.T) {
	srv, _ := consoleTestServer(t)
	token := bootstrapAdmin(t, srv)

	if w := do(t, srv, "PUT", "/api/console/users/nikhil/password", token,
		map[string]string{"password": "brand-new-one"}); w.Code != http.StatusForbidden {
		t.Errorf("a password change without the current one was allowed: %d", w.Code)
	}
	w := do(t, srv, "PUT", "/api/console/users/nikhil/password", token,
		map[string]string{"current_password": "correct-horse", "password": "brand-new-one"})
	if w.Code != http.StatusOK {
		t.Fatalf("a correct password change failed: %s", w.Body.String())
	}
	// The session it was made from is gone too, and the response says so.
	if !strings.Contains(w.Body.String(), "sign_in_again") {
		t.Error("the response does not warn that the session was revoked")
	}
	if got := do(t, srv, "GET", "/api/console/users", token, nil); got.Code != http.StatusUnauthorized {
		t.Error("the old session survived a password change")
	}
}

// The context is added to every message the agents handle, so its length is a
// running cost, not a one-off.
func TestTheOrgContextIsCapped(t *testing.T) {
	srv, _ := consoleTestServer(t)
	token := bootstrapAdmin(t, srv)

	w := do(t, srv, "PUT", "/api/console/organisation", token,
		map[string]string{"name": "Acme", "context": strings.Repeat("x", 9000)})
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("an oversized context was accepted: %d", w.Code)
	}

	w = do(t, srv, "PUT", "/api/console/organisation", token, map[string]string{
		"name": "Zero Moblt", "domain": "zeromoblt.com", "context": "Tickets live in YouTrack.",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("a normal profile was refused: %s", w.Body.String())
	}
	// The briefing is returned so whoever writes the context sees the result
	// rather than guessing at it.
	if !strings.Contains(w.Body.String(), "You work for Zero Moblt") {
		t.Errorf("the response does not show what the agents will be told: %s", w.Body.String())
	}
}

// The callback path used to be invented as "/hooks/"+id. GitHub's real path is
// /connectors/github, so the wizard printed a URL that had never existed —
// worse than showing nothing, because it looked finished.
func TestTheCallbackURLIsTheConnectorsRealPath(t *testing.T) {
	srv, _ := consoleTestServer(t)
	srv.conns = connectors.NewHost(nil, nil, nil, zap.NewNop())
	srv.conns.Register(githubconn.New(""))
	srv.cfg = &config.KarmaxConfig{}
	srv.cfg.Console.PublicURL = "https://console.example"

	got := srv.callbackURL("github")
	if got != "https://console.example/connectors/github" {
		t.Errorf("callback URL is %q — it must match the path the connector actually serves", got)
	}
	if strings.Contains(got, "/hooks/") {
		t.Error("the invented /hooks/ path is back")
	}
}

// The webhook server and the console are usually different addresses.
func TestTheWebhookHostCanDifferFromTheConsole(t *testing.T) {
	srv, _ := consoleTestServer(t)
	srv.conns = connectors.NewHost(nil, nil, nil, zap.NewNop())
	srv.conns.Register(githubconn.New(""))
	srv.cfg = &config.KarmaxConfig{}
	srv.cfg.Console.PublicURL = "https://console.example"
	srv.cfg.Webhooks.PublicURL = "https://hooks.example"

	if got := srv.callbackURL("github"); got != "https://hooks.example/connectors/github" {
		t.Errorf("webhooks.public_url was not preferred: %q", got)
	}
}

// A connector with no webhook source has no callback URL, and saying so by
// omission beats inventing one.
func TestAConnectorWithoutWebhooksHasNoCallbackURL(t *testing.T) {
	srv, _ := consoleTestServer(t)
	srv.conns = connectors.NewHost(nil, nil, nil, zap.NewNop())
	srv.conns.Register(slackconn.New()) // Sources() returns nil
	srv.cfg = &config.KarmaxConfig{}
	srv.cfg.Console.PublicURL = "https://console.example"

	if got := srv.callbackURL("slack"); got != "" {
		t.Errorf("invented a callback URL for a connector with no webhook: %q", got)
	}
}

// The setup response must surface it in both places the console reads.
func TestSetupSurfacesTheCallbackURL(t *testing.T) {
	srv, _ := consoleTestServer(t)
	srv.conns = connectors.NewHost(nil, nil, nil, zap.NewNop())
	srv.conns.Register(githubconn.New(""))
	srv.cfg = &config.KarmaxConfig{}
	srv.cfg.Console.PublicURL = "https://console.example"
	token := bootstrapAdmin(t, srv)

	w := do(t, srv, "GET", "/api/console/connectors/github/setup", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var body struct {
		CallbackURL string `json:"callback_url"`
		Steps       []struct {
			Title string `json:"title"`
			Value string `json:"value"`
		} `json:"steps"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)

	want := "https://console.example/connectors/github"
	if body.CallbackURL != want {
		t.Errorf("callback_url is %q, want %q", body.CallbackURL, want)
	}
	var onAStep bool
	for _, s := range body.Steps {
		if s.Value == want {
			onAStep = true
		}
	}
	if !onAStep {
		t.Error("no setup step carries the callback URL to copy")
	}
}

// Someone about to paste a callback into GitHub should be told it is plaintext
// at that moment, not left to notice the scheme.
func TestAPlainHTTPCallbackIsFlagged(t *testing.T) {
	srv, _ := consoleTestServer(t)
	srv.conns = connectors.NewHost(nil, nil, nil, zap.NewNop())
	srv.conns.Register(githubconn.New(""))
	srv.cfg = &config.KarmaxConfig{}
	srv.cfg.Webhooks.PublicURL = "http://13.207.76.239:9090"
	token := bootstrapAdmin(t, srv)

	w := do(t, srv, "GET", "/api/console/connectors/github/setup", token, nil)
	if !strings.Contains(w.Body.String(), "not HTTPS") {
		t.Error("a plaintext callback URL was offered with no warning")
	}

	// And an HTTPS one must not be nagged about.
	srv.cfg.Webhooks.PublicURL = "https://hooks.example"
	w = do(t, srv, "GET", "/api/console/connectors/github/setup", token, nil)
	if strings.Contains(w.Body.String(), "not HTTPS") {
		t.Error("an HTTPS callback URL was flagged anyway")
	}
}
