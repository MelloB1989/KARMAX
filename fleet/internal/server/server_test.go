package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

type fake struct {
	db    *ledger.DB
	calls []string
}

func (f *fake) Status(time.Time) []daemon.Row {
	return []daemon.Row{{Agent: "agent-03", State: "standby"}}
}
func (f *fake) Tell(_ context.Context, from, to, text string, _ time.Time) error {
	f.calls = append(f.calls, "tell "+from+" "+to+" "+text)
	return nil
}
func (f *fake) Assign(_ context.Context, name, task, from, brief string, _ time.Time) error {
	f.calls = append(f.calls, "assign "+name+" "+task+" "+from+" "+brief)
	return nil
}
func (f *fake) Inbox(after int64) ([]ledger.Relay, error) {
	return []ledger.Relay{{ID: after + 1, From: "agent-03", To: "operator", Text: "ready"}}, nil
}
func (f *fake) Wait(ctx context.Context, agent, task string, after int64) (daemon.WaitResult, error) {
	f.calls = append(f.calls, fmt.Sprintf("wait %s %s %d", agent, task, after))
	if _, ok := ctx.Deadline(); !ok {
		return daemon.WaitResult{}, fmt.Errorf("wait without a deadline")
	}
	return daemon.WaitResult{Reason: "message", Message: &ledger.Relay{ID: after + 1, From: agent, Text: "ready"}}, nil
}
func (f *fake) Done(name, task, outcome, branch, pr string, _ time.Time) error {
	f.calls = append(f.calls, "done "+name+" "+task+" "+outcome+" "+branch)
	return nil
}
func (f *fake) Rotate(name string, _ time.Time) error {
	f.calls = append(f.calls, "rotate "+name)
	return nil
}
func (f *fake) Enable(name string) error { f.calls = append(f.calls, "enable "+name); return nil }
func (f *fake) Restore(_ context.Context, id, onto string, _ time.Time) error {
	f.calls = append(f.calls, "restore "+id+" "+onto)
	return nil
}
func (f *fake) DB() *ledger.DB      { return f.db }
func (f *fake) ArchiveRoot() string { return "/nonexistent" }

func setup(t *testing.T) (*fake, http.Handler) {
	t.Helper()
	db, err := ledger.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &fake{db: db}
	return f, Handler(f, Tokens{Full: "full", Relay: "relay", Console: "console"}, nil)
}

func do(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestScopes(t *testing.T) {
	_, h := setup(t)
	cases := []struct {
		method, path, token, body string
		want                      int
	}{
		{"GET", "/v1/status", "", "", 401},
		{"GET", "/v1/status", "wrong", "", 401},
		{"GET", "/v1/status", "relay", "", 200},
		{"POST", "/v1/tell", "relay", `{"from":"agent-05","to":"agent-03","text":"hi"}`, 200},
		// The operator's authority is not an agent's to claim.
		{"POST", "/v1/tell", "relay", `{"from":"operator","to":"agent-03","text":"hi"}`, 403},
		{"POST", "/v1/tell", "full", `{"from":"operator","to":"agent-03","text":"hi"}`, 200},
		{"POST", "/v1/assign", "relay", `{"agent":"agent-03","task":"T1"}`, 403},
		{"POST", "/v1/restore", "relay", `{"archive":"a/b"}`, 403},
		{"POST", "/v1/assign", "full", `{"agent":"agent-03","task":"T1"}`, 200},
		{"POST", "/v1/assign", "console", `{"agent":"agent-03","task":"T1"}`, 403},
		{"POST", "/v1/done", "console", `{"agent":"agent-03"}`, 403},
		{"POST", "/v1/rotate", "console", `{"agent":"agent-03"}`, 200},
		{"POST", "/v1/rotate", "relay", `{"agent":"agent-03"}`, 403},
		{"GET", "/v1/events?agent=agent-03&since=2h", "console", "", 200},
		{"GET", "/v1/inbox", "console", "", 200},
		{"GET", "/v1/inbox", "relay", "", 403},
		// Agents report to the operator; they still cannot speak as one.
		{"POST", "/v1/tell", "relay", `{"from":"agent-03","to":"operator","text":"ready"}`, 200},
		{"GET", "/v1/events?agent=agent-03&since=2h", "full", "", 200},
		{"GET", "/v1/events?agent=agent-03&since=2h", "relay", "", 403},
	}
	for _, c := range cases {
		if rec := do(t, h, c.method, c.path, c.token, c.body); rec.Code != c.want {
			t.Errorf("%s %s as %q: %d (%s), want %d", c.method, c.path, c.token, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
	}
}

func TestCallsReachTheFleet(t *testing.T) {
	f, h := setup(t)
	do(t, h, "POST", "/v1/done", "full", `{"agent":"agent-03","task":"T1","outcome":"done","branch":"fleet/agent-03/x"}`)
	do(t, h, "POST", "/v1/rotate", "full", `{"agent":"agent-03"}`)
	do(t, h, "POST", "/v1/enable", "full", `{"agent":"agent-03"}`)
	do(t, h, "POST", "/v1/restore", "full", `{"archive":"agent-03/2026-10-08-abcd","agent":"agent-04"}`)
	want := "done agent-03 T1 done fleet/agent-03/x|rotate agent-03|enable agent-03|restore agent-03/2026-10-08-abcd agent-04"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("calls = %s", got)
	}
}

func TestBadRequests(t *testing.T) {
	_, h := setup(t)
	for _, body := range []string{`{"to":"agent-03"}`, `not json`, `{"from":"a","to":"b","text":""}`} {
		if rec := do(t, h, "POST", "/v1/tell", "full", body); rec.Code != 400 {
			t.Errorf("tell %s: %d", body, rec.Code)
		}
	}
}

func TestStatusShape(t *testing.T) {
	_, h := setup(t)
	rec := do(t, h, "GET", "/v1/status", "full", "")
	var out struct {
		Agents []daemon.Row `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Agents) != 1 || out.Agents[0].Agent != "agent-03" {
		t.Fatalf("body = %s", rec.Body)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"2h": now.Add(-2 * time.Hour), "7d": now.Add(-7 * 24 * time.Hour), "30m": now.Add(-30 * time.Minute),
		"2026-10-01T00:00:00Z": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "": {},
	} {
		got, err := ParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseSince(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseSince("soon", now); err == nil {
		t.Error("want an error")
	}
}

func TestForbiddenNamesWhatTheTokenMay(t *testing.T) {
	_, h := setup(t)
	rec := do(t, h, "POST", "/v1/assign", "console", `{"agent":"agent-03","task":"T1"}`)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "console") || !strings.Contains(rec.Body.String(), "POST /v1/rotate") {
		t.Fatalf("403 body = %s", rec.Body)
	}
}

func TestAConsoleTellSpeaksAsTheOperator(t *testing.T) {
	f, h := setup(t)
	do(t, h, "POST", "/v1/tell", "console", `{"from":"agent-05","to":"agent-03","text":"hi"}`)
	if got := f.calls[len(f.calls)-1]; got != "tell operator agent-03 hi" {
		t.Fatalf("call = %q", got)
	}
}

func TestIndexListsEveryRouteWithItsScopes(t *testing.T) {
	_, h := setup(t)
	rec := do(t, h, "GET", "/v1", "relay", "")
	var idx []struct {
		Route  string   `json:"route"`
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &idx); err != nil || len(idx) < 14 {
		t.Fatalf("index = %s", rec.Body)
	}
	for _, r := range idx {
		if r.Route == "POST /v1/assign" && strings.Join(r.Scopes, ",") != "full" {
			t.Fatalf("assign scopes = %v", r.Scopes)
		}
	}
}

func TestAssignPassesFromAndBriefAndDefaultsFromToOperator(t *testing.T) {
	f, h := setup(t)
	do(t, h, "POST", "/v1/assign", "full", `{"agent":"agent-03","task":"T1","brief":"build it"}`)
	do(t, h, "POST", "/v1/assign", "full", `{"agent":"agent-03","task":"T2","from":"mira"}`)
	want := "assign agent-03 T1 operator build it|assign agent-03 T2 mira "
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("calls = %q", got)
	}
}

func TestInboxRouteReturnsOperatorMessagesAfterAnID(t *testing.T) {
	_, h := setup(t)
	rec := do(t, h, "GET", "/v1/inbox?after=41", "full", "")
	var msgs []ledger.Relay
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil || len(msgs) != 1 || msgs[0].ID != 42 {
		t.Fatalf("inbox = %s", rec.Body)
	}
}

func TestWaitRouteGivesTheWaitADeadline(t *testing.T) {
	f, h := setup(t)
	rec := do(t, h, "GET", "/v1/wait?agent=agent-03&task=T1&after=7&timeout=2m", "console", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"reason":"message"`) {
		t.Fatalf("wait = %d %s", rec.Code, rec.Body)
	}
	if got := f.calls[len(f.calls)-1]; got != "wait agent-03 T1 7" {
		t.Fatalf("call = %q", got)
	}
	if rec := do(t, h, "GET", "/v1/wait?agent=agent-03&timeout=soon", "full", ""); rec.Code != 400 {
		t.Fatalf("bad timeout = %d", rec.Code)
	}
}

func TestWhoamiNamesTheScope(t *testing.T) {
	_, h := setup(t)
	for tok, want := range map[string]string{"full": `"scope":"full"`, "relay": `"scope":"relay"`, "console": `"scope":"console"`} {
		if rec := do(t, h, "GET", "/v1/whoami", tok, ""); !strings.Contains(rec.Body.String(), want) {
			t.Errorf("whoami as %s = %s", tok, rec.Body)
		}
	}
}

func TestHealthzNeedsNoToken(t *testing.T) {
	_, h := setup(t)
	rec := do(t, h, "GET", "/healthz", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"version"`) {
		t.Fatalf("healthz = %d %s", rec.Code, rec.Body)
	}
}
