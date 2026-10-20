package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/archive"
	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

type fake struct {
	calls []string
}

func (f *fake) Status(time.Time) []daemon.Row {
	return []daemon.Row{{Agent: "agent-03", State: "standby", Task: "T-ram"}, {Agent: "agent-02", State: "working", Task: "T1"}}
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
	if task == "T-missing" {
		return daemon.WaitResult{}, fmt.Errorf("%w %s", daemon.ErrNoTask, task)
	}
	if _, ok := ctx.Deadline(); !ok {
		return daemon.WaitResult{}, fmt.Errorf("wait without a deadline")
	}
	return daemon.WaitResult{Reason: "message", Message: &ledger.Relay{ID: after + 1, From: agent, Text: "ready"}}, nil
}
func (f *fake) Result(task string) (daemon.TaskResult, error) {
	f.calls = append(f.calls, "result "+task)
	if task == "T-missing" {
		return daemon.TaskResult{}, fmt.Errorf("%w %s", daemon.ErrNoTask, task)
	}
	return daemon.TaskResult{Task: ledger.Task{ID: task, Agent: "agent-03"}, State: "open", Reports: []ledger.Relay{}}, nil
}
func (f *fake) Done(name, task, outcome, branch, pr string, _ time.Time) error {
	f.calls = append(f.calls, "done "+name+" "+task+" "+outcome+" "+branch)
	return nil
}
func (f *fake) Close(name, task, outcome, branch, pr string, _ time.Time) error {
	f.calls = append(f.calls, "close "+name+" "+task+" "+outcome+" "+branch)
	return nil
}
func (f *fake) Followup(_ context.Context, task, from, text string, _ time.Time) error {
	f.calls = append(f.calls, "followup "+task+" "+from+" "+text)
	if task == "T-closed" {
		return fmt.Errorf("%w: task %s is closed", daemon.ErrConflict, task)
	}
	if task == "T-away" {
		return &daemon.Held{To: "agent-03", Cause: fmt.Errorf("its host pc2 does not answer")}
	}
	if task == "T-missing" {
		return fmt.Errorf("%w %s", daemon.ErrNoTask, task)
	}
	return nil
}
func (f *fake) Cancel(_ context.Context, task, from, reason string, _ time.Time) error {
	f.calls = append(f.calls, "cancel "+task+" "+from+" "+reason)
	if task == "T-closed" {
		return fmt.Errorf("%w: task %s is closed", daemon.ErrConflict, task)
	}
	return nil
}
func (f *fake) Rotate(name string, dry bool, _ time.Time) (daemon.RotatePlan, error) {
	f.calls = append(f.calls, "rotate "+name+map[bool]string{true: " dry"}[dry])
	return daemon.RotatePlan{Closes: "T9", When: "next idle tick"}, nil
}
func (f *fake) Enable(name string) error { f.calls = append(f.calls, "enable "+name); return nil }
func (f *fake) Restore(_ context.Context, id, onto string, _ time.Time) error {
	f.calls = append(f.calls, "restore "+id+" "+onto)
	return nil
}
func (f *fake) Events(q daemon.EventsQuery) ([]ledger.Event, error) {
	f.calls = append(f.calls, fmt.Sprintf("events agent=%q after=%d kinds=%v limit=%d newest=%v since0=%v",
		q.Agent, q.After, q.Kinds, q.Limit, q.Newest, q.Since.IsZero()))
	return []ledger.Event{{ID: 7, Agent: "agent-03", Kind: "Stop", Detail: `{"a":1}`}}, nil
}
func (f *fake) Usage(by string, _ time.Time) ([]ledger.UsageRow, error) {
	return []ledger.UsageRow{}, nil
}
func (f *fake) History(agent string, n int) ([]ledger.Session, error) {
	return []ledger.Session{{ID: "s", Agent: agent, CostUSD: 1}}, nil
}
func (f *fake) Quota() (map[string]ledger.Quota, error) { return map[string]ledger.Quota{}, nil }
func (f *fake) Tasks(open bool, agent, assigner string, n int) ([]ledger.Task, error) {
	call := fmt.Sprintf("tasks open=%v agent=%q n=%d", open, agent, n)
	if assigner != "" {
		call += " by=" + assigner
	}
	f.calls = append(f.calls, call)
	return []ledger.Task{{ID: "T1", Agent: "agent-03", Assigner: "operator"}}, nil
}

// TaskAgent: T-ram is a tailnet member's task; T-missing does not exist;
// every other is the operator's.
func (f *fake) TaskAgent(task string) (*ledger.Task, error) {
	switch task {
	case "T-missing":
		return nil, fmt.Errorf("%w %s", daemon.ErrNoTask, task)
	case "T-ram":
		return &ledger.Task{ID: task, Agent: "agent-03", Assigner: "ram@example.com"}, nil
	}
	return &ledger.Task{ID: task, Agent: "agent-03", Assigner: "operator"}, nil
}
func (f *fake) Activity(_ time.Time, b time.Duration, _ time.Time) (daemon.ActivityReply, error) {
	f.calls = append(f.calls, "activity "+b.String())
	return daemon.ActivityReply{BucketS: 900, Agents: map[string][]ledger.Bucket{"agent-03": {{N: 3}}}}, nil
}
func (f *fake) Today(time.Time) (daemon.TodayReply, error) {
	return daemon.TodayReply{TasksClosed: 2, SpendUSD: 0.5}, nil
}
func (f *fake) Peek(_ context.Context, agent string, n int) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("peek %s %d", agent, n))
	return "screen", nil
}
func (f *fake) Prompt(_ context.Context, agent, text string) error {
	f.calls = append(f.calls, "prompt "+agent+" "+text)
	return nil
}
func (f *fake) Archives(time.Time) ([]*archive.Manifest, error) { return []*archive.Manifest{}, nil }
func (f *fake) Archive(id string) (*daemon.ArchiveDetail, error) {
	f.calls = append(f.calls, "archive "+id)
	return &daemon.ArchiveDetail{Manifest: &archive.Manifest{ID: id}, TranscriptLines: 12}, nil
}
func (f *fake) PruneArchives(time.Time) (int, error) {
	f.calls = append(f.calls, "prune")
	return 4, nil
}
func (f *fake) Render() (string, error) { f.calls = append(f.calls, "render"); return "rendered", nil }
func (f *fake) Up(_ context.Context, h []string) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("up %v", h))
	return "$ up", nil
}
func (f *fake) Down(_ context.Context, h []string) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("down %v", h))
	return "", fmt.Errorf("boom")
}

func setup(t *testing.T) (*fake, http.Handler) {
	t.Helper()
	f := &fake{}
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
		{"POST", "/v1/tell", "relay", `{"from":"ram@example.com","to":"agent-03","text":"hi"}`, 403},
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
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Agents) != 2 || out.Agents[0].Agent != "agent-03" {
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

func TestWaitRouteTakesATaskWithoutAnAgent(t *testing.T) {
	f, h := setup(t)
	if rec := do(t, h, "GET", "/v1/wait?task=T1&timeout=2m", "full", ""); rec.Code != 200 {
		t.Fatalf("wait by task = %d %s", rec.Code, rec.Body)
	}
	if got := f.calls[len(f.calls)-1]; got != "wait  T1 0" {
		t.Fatalf("call = %q", got)
	}
	if rec := do(t, h, "GET", "/v1/wait?timeout=2m", "full", ""); rec.Code != 400 {
		t.Fatalf("wait with neither = %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/v1/wait?task=T-missing", "full", ""); rec.Code != 404 {
		t.Fatalf("wait on unknown task = %d", rec.Code)
	}
}

func TestTaskResultRoute(t *testing.T) {
	f, h := setup(t)
	rec := do(t, h, "GET", "/v1/tasks/T1", "full", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"open"`) || !strings.Contains(rec.Body.String(), `"reports":[]`) {
		t.Fatalf("result = %d %s", rec.Code, rec.Body)
	}
	if got := f.calls[len(f.calls)-1]; got != "result T1" {
		t.Fatalf("call = %q", got)
	}
	if rec := do(t, h, "GET", "/v1/tasks/T-missing", "full", ""); rec.Code != 404 {
		t.Fatalf("unknown task = %d", rec.Code)
	}
	for tok, want := range map[string]int{"console": 200, "relay": 403, "": 401} {
		if rec := do(t, h, "GET", "/v1/tasks/T1", tok, ""); rec.Code != want {
			t.Errorf("as %q: %d, want %d", tok, rec.Code, want)
		}
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

func TestDoneKeepSessionCallsCloseNotDone(t *testing.T) {
	f, h := setup(t)
	if w := do(t, h, "POST", "/v1/done", "full", `{"agent":"agent-03","task":"T1","outcome":"superseded","keep_session":true}`); w.Code != 200 {
		t.Fatalf("done keep_session = %d %s", w.Code, w.Body)
	}
	if got := f.calls[len(f.calls)-1]; got != "close agent-03 T1 superseded " {
		t.Fatalf("call = %q", got)
	}
	_ = do(t, h, "POST", "/v1/done", "full", `{"agent":"agent-03","task":"T1"}`)
	if got := f.calls[len(f.calls)-1]; !strings.HasPrefix(got, "done agent-03 T1") {
		t.Fatalf("without keep_session: %q", got)
	}
}

func TestEventsRouteTakesACursorKindsAndNewest(t *testing.T) {
	f, h := setup(t)
	rec := do(t, h, "GET", "/v1/events?after=41&kinds=Stop,fleet.&limit=5", "console", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":7`) || !strings.Contains(rec.Body.String(), `"detail":{"a":1}`) {
		t.Fatalf("events = %d %s", rec.Code, rec.Body)
	}
	do(t, h, "GET", "/v1/events?newest=30&kinds=Stop", "console", "")
	do(t, h, "GET", "/v1/events", "console", "")
	want := `events agent="" after=41 kinds=[Stop fleet.] limit=5 newest=false since0=true` + "|" +
		`events agent="" after=0 kinds=[Stop] limit=30 newest=true since0=false` + "|" +
		`events agent="" after=0 kinds=[] limit=0 newest=false since0=false`
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
	}
	if rec := do(t, h, "GET", "/v1/events?after=x", "console", ""); rec.Code != 400 {
		t.Errorf("after=x = %d, want 400", rec.Code)
	}
}

func TestReadRoutesHaveTheShapesTheConsolePolls(t *testing.T) {
	f, h := setup(t)
	for path, want := range map[string]string{
		"/v1/tasks?open=1":                  `"assigner":"operator"`,
		"/v1/activity?since=24h&bucket=15m": `"bucket_s":900`,
		"/v1/today":                         `"tasks_closed":2`,
		"/v1/peek?agent=agent-03&n=10":      `"text":"screen"`,
		"/v1/history?agent=agent-03&n=20":   `"cost_usd":1`,
	} {
		for _, tok := range []string{"console", "full"} {
			if rec := do(t, h, "GET", path, tok, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
				t.Errorf("%s as %s = %d %s, want it to contain %s", path, tok, rec.Code, rec.Body, want)
			}
		}
		if rec := do(t, h, "GET", path, "relay", ""); rec.Code != 403 {
			t.Errorf("%s as relay = %d, want 403", path, rec.Code)
		}
	}
	if !strings.Contains(strings.Join(f.calls, "|"), `tasks open=true agent="" n=0`) ||
		!strings.Contains(strings.Join(f.calls, "|"), "activity 15m0s") || !strings.Contains(strings.Join(f.calls, "|"), "peek agent-03 10") {
		t.Errorf("calls = %v", f.calls)
	}
	if rec := do(t, h, "GET", "/v1/peek", "full", ""); rec.Code != 400 {
		t.Errorf("peek without agent = %d", rec.Code)
	}
}

func TestPromptRouteIsForTheConsoleAndFull(t *testing.T) {
	f, h := setup(t)
	if rec := do(t, h, "POST", "/v1/prompt", "console", `{"agent":"agent-03","text":"hi"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("prompt = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/v1/prompt", "relay", `{"agent":"agent-03","text":"hi"}`); rec.Code != 403 {
		t.Errorf("relay prompt = %d, want 403", rec.Code)
	}
	if rec := do(t, h, "POST", "/v1/prompt", "full", `{"agent":"agent-03"}`); rec.Code != 400 {
		t.Errorf("prompt without text = %d, want 400", rec.Code)
	}
	if got := strings.Join(f.calls, "|"); got != "prompt agent-03 hi" {
		t.Errorf("calls = %s", got)
	}
}

func TestRotateDryRunReplyNamesWhatItCloses(t *testing.T) {
	f, h := setup(t)
	rec := do(t, h, "POST", "/v1/rotate", "console", `{"agent":"agent-03","dry_run":true}`)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got["closes"] != "T9" || got["when"] != "next idle tick" || got["dry_run"] != true || got["ok"] != true {
		t.Fatalf("dry run = %d %s", rec.Code, rec.Body)
	}
	do(t, h, "POST", "/v1/rotate", "console", `{"agent":"agent-03"}`)
	if got := strings.Join(f.calls, "|"); got != "rotate agent-03 dry|rotate agent-03" {
		t.Errorf("calls = %s", got)
	}
}

func TestArchiveRoutesAreFullOnly(t *testing.T) {
	f, h := setup(t)
	rec := do(t, h, "GET", "/v1/archives/agent-03/2026-10-08-abcd", "full", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"transcript_lines":12`) || !strings.Contains(rec.Body.String(), `"id":"agent-03/2026-10-08-abcd"`) {
		t.Fatalf("archive = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/v1/archives/agent-03/x", "console", ""); rec.Code != 403 {
		t.Errorf("console archive read = %d, want 403", rec.Code)
	}
	rec = do(t, h, "DELETE", "/v1/archives?older=180d", "full", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"removed":4}` {
		t.Fatalf("prune = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "DELETE", "/v1/archives", "full", ""); rec.Code != 400 {
		t.Errorf("prune without older = %d, want 400", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/v1/archives?older=1d", "console", ""); rec.Code != 403 {
		t.Errorf("console prune = %d, want 403", rec.Code)
	}
	_ = f
}

func TestHostRoutesAreFullOnlyAndReportOutputAndFailure(t *testing.T) {
	f, h := setup(t)
	rec := do(t, h, "POST", "/v1/up", "full", `{"hosts":["pc2"]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"output":"$ up"`) || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("up = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/v1/render", "full", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "rendered") {
		t.Errorf("render = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/v1/down", "full", ""); rec.Code != 500 || !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("a failing down = %d %s, want 500 naming the error", rec.Code, rec.Body)
	}
	for _, p := range []string{"/v1/up", "/v1/down", "/v1/render"} {
		for _, tok := range []string{"console", "relay"} {
			if rec := do(t, h, "POST", p, tok, ""); rec.Code != 403 {
				t.Errorf("%s as %s = %d, want 403", p, tok, rec.Code)
			}
		}
	}
	if got := strings.Join(f.calls, "|"); !strings.HasPrefix(got, "up [pc2]|render|down []") {
		t.Errorf("calls = %s", got)
	}
}

func TestWaitOnAnUnknownTaskIs404WithTheTaskNamed(t *testing.T) {
	h := Handler(&fake{}, Tokens{Full: "full"}, nil)
	rec := do(t, h, "GET", "/v1/wait?agent=agent-03&task=T-missing&timeout=1m", "full", "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no task T-missing") {
		t.Fatalf("= %d %s", rec.Code, rec.Body)
	}
}

func TestGuideRouteScopes(t *testing.T) {
	_, h := setup(t)
	for tok, want := range map[string]int{"": 401, "relay": 403, "console": 200, "full": 200} {
		if rec := do(t, h, "GET", "/v1/guide", tok, ""); rec.Code != want {
			t.Errorf("guide as %q = %d, want %d", tok, rec.Code, want)
		}
	}
	var out struct{ Text string }
	if err := json.Unmarshal(do(t, h, "GET", "/v1/guide", "full", "").Body.Bytes(), &out); err != nil || !strings.Contains(out.Text, "fleetctl run") {
		t.Fatalf("guide body = %q, %v", out.Text, err)
	}
}

func TestFollowupAndCancelRoutes(t *testing.T) {
	f, h := setup(t)
	for _, tok := range []string{"relay", "console"} {
		if w := do(t, h, "POST", "/v1/tasks/T1/followup", tok, `{"text":"x"}`); w.Code != 403 {
			t.Errorf("followup as %s = %d", tok, w.Code)
		}
		if w := do(t, h, "POST", "/v1/tasks/T1/cancel", tok, `{}`); w.Code != 403 {
			t.Errorf("cancel as %s = %d", tok, w.Code)
		}
	}
	if w := do(t, h, "POST", "/v1/tasks/T1/followup", "full", `{"from":"karmax","text":"fix it"}`); w.Code != 200 {
		t.Fatalf("followup = %d %s", w.Code, w.Body)
	}
	if got := f.calls[len(f.calls)-1]; got != "followup T1 karmax fix it" {
		t.Fatalf("call = %q", got)
	}
	do(t, h, "POST", "/v1/tasks/T1/followup", "full", `{"text":"hi"}`)
	if got := f.calls[len(f.calls)-1]; got != "followup T1 operator hi" {
		t.Fatalf("default sender: %q", got)
	}
	if w := do(t, h, "POST", "/v1/tasks/T1/followup", "full", `{"text":"  "}`); w.Code != 400 {
		t.Fatalf("empty text = %d", w.Code)
	}
	if w := do(t, h, "POST", "/v1/tasks/T-closed/followup", "full", `{"text":"x"}`); w.Code != 409 {
		t.Fatalf("closed followup = %d", w.Code)
	}
	if w := do(t, h, "POST", "/v1/tasks/T-missing/followup", "full", `{"text":"x"}`); w.Code != 404 {
		t.Fatalf("unknown followup = %d", w.Code)
	}
	// Held for an agent out of reach: taken, and said so.
	w := do(t, h, "POST", "/v1/tasks/T-away/followup", "full", `{"text":"x"}`)
	var q struct {
		OK, Queued bool
		Why        string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &q); w.Code != 202 || err != nil || !q.OK || !q.Queued || !strings.Contains(q.Why, "pc2 does not answer") {
		t.Fatalf("held followup = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/v1/tasks/T1/cancel", "full", `{"from":"karmax","reason":"wrong repo"}`); w.Code != 200 {
		t.Fatalf("cancel = %d %s", w.Code, w.Body)
	}
	if got := f.calls[len(f.calls)-1]; got != "cancel T1 karmax wrong repo" {
		t.Fatalf("call = %q", got)
	}
	if w := do(t, h, "POST", "/v1/tasks/T-closed/cancel", "full", `{}`); w.Code != 409 {
		t.Fatalf("closed cancel = %d", w.Code)
	}
}

func TestDoneByTaskNeedsAgentOrTask(t *testing.T) {
	f, h := setup(t)
	if w := do(t, h, "POST", "/v1/done", "full", `{"task":"T1"}`); w.Code != 200 {
		t.Fatalf("done by task = %d %s", w.Code, w.Body)
	}
	if got := f.calls[len(f.calls)-1]; !strings.HasPrefix(got, "done  T1") {
		t.Fatalf("call = %q", got)
	}
	if w := do(t, h, "POST", "/v1/done", "full", `{}`); w.Code != 400 {
		t.Fatalf("done with nothing = %d", w.Code)
	}
}
