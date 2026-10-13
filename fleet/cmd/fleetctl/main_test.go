package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFleetd records requests and answers each path with a canned body.
type fakeFleetd struct {
	mu    sync.Mutex
	reqs  []string
	reply map[string]string
}

func (f *fakeFleetd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI()+" "+strings.TrimSpace(string(b)))
	body, ok := f.reply[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		body = `{"ok":true}`
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

// hostYAML, when a test sets it, makes the next fleetctl() calls run on a fleet host.
var hostYAML string

// fleetctl runs one command against a fake fleetd and returns its stdout.
func fleetctl(t *testing.T, f *fakeFleetd, args ...string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("FLEETD_URL", srv.URL)
	t.Setenv("FLEET_TOKEN", "tok")
	t.Setenv("FLEET_NAME", "")
	t.Setenv("FLEET_AGENT", "")
	cfg := hostYAML
	if cfg == "" {
		cfg = t.TempDir() + "/none.yaml" // not on a fleet host: everything goes through fleetd
	}
	t.Setenv("FLEET_CONFIG", cfg)
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := run(args[0], args[1:])
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), err
}

func TestGiveRecordsTheTaskAndDeliversTheBrief(t *testing.T) {
	f := &fakeFleetd{}
	if _, err := fleetctl(t, f, "give", "agent-03", "--task", "T1", "build", "the", "thing"); err != nil {
		t.Fatal(err)
	}
	var in map[string]string
	_ = json.Unmarshal([]byte(strings.SplitN(f.reqs[0], " ", 3)[2]), &in)
	if !strings.HasPrefix(f.reqs[0], "POST /v1/assign ") || in["agent"] != "agent-03" || in["task"] != "T1" ||
		in["from"] != "operator" || in["brief"] != "Task T1 from operator. build the thing" {
		t.Fatalf("request = %s", f.reqs[0])
	}
}

func TestGiveNeedsATaskAndABrief(t *testing.T) {
	for _, args := range [][]string{{"give", "agent-03", "do it"}, {"give", "agent-03", "--task", "T1"}} {
		if _, err := fleetctl(t, &fakeFleetd{}, args...); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
}

func TestInboxPrintsMessages(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/inbox": `[{"id":4,"from":"agent-03","to":"operator","text":"ready on fleet/agent-03/x","at":"2026-10-11T14:00:00Z"}]`}}
	out, err := fleetctl(t, f, "inbox", "--after", "3")
	if err != nil || !strings.Contains(out, "agent-03") || !strings.Contains(out, "ready on fleet/agent-03/x") || !strings.Contains(out, "#4") {
		t.Fatalf("out = %q, %v", out, err)
	}
	if !strings.HasPrefix(f.reqs[0], "GET /v1/inbox?after=3 ") {
		t.Fatalf("request = %s", f.reqs[0])
	}
}

func TestWaitStartsAfterTheNewestMessageAndPrintsTheReport(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{
		"/v1/inbox": `[{"id":9,"from":"agent-05","text":"old"}]`,
		"/v1/wait":  `{"reason":"message","message":{"id":10,"from":"agent-03","text":"ready"}}`,
	}}
	out, err := fleetctl(t, f, "wait", "agent-03", "--timeout", "10m")
	if err != nil || !strings.Contains(out, "ready") {
		t.Fatalf("out = %q, %v", out, err)
	}
	if got := f.reqs[1]; !strings.HasPrefix(got, "GET /v1/wait?") || !strings.Contains(got, "after=9") ||
		!strings.Contains(got, "agent=agent-03") || !strings.Contains(got, "timeout=10m") {
		t.Fatalf("request = %s", got)
	}
}

// A task's reports are its own: a wait on one reads them from the start, so
// a report that arrived before the wait began is not lost.
func TestWaitByTaskWithoutAnAgentSendsNoAgentParam(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{
		"/v1/wait": `{"reason":"message","message":{"id":10,"from":"agent-03","to":"karmax","text":"ready"}}`,
	}}
	out, err := fleetctl(t, f, "wait", "--task", "T1", "--timeout", "10m")
	if err != nil || !strings.Contains(out, "ready") {
		t.Fatalf("out = %q, %v", out, err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("requests = %v", f.reqs)
	}
	got := f.reqs[0]
	if !strings.HasPrefix(got, "GET /v1/wait?") || strings.Contains(got, "agent=") || !strings.Contains(got, "task=T1") ||
		!strings.Contains(got, "after=0") {
		t.Fatalf("request = %s", got)
	}
	if _, err := fleetctl(t, f, "wait", "--task", "T1", "--after", "10"); err != nil || !strings.Contains(f.reqs[1], "after=10") {
		t.Fatalf("requests = %v, %v", f.reqs, err)
	}
}

func TestWaitNeedsAnAgentOrATask(t *testing.T) {
	if _, err := fleetctl(t, &fakeFleetd{}, "wait"); err == nil {
		t.Fatal("wait with neither should fail")
	}
}

func TestResultPrintsTheTaskReportsAndCommits(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/tasks/T1": `{
	  "task":{"id":"T1","agent":"agent-03","assigner":"karmax","outcome":"done","branch":"fleet/agent-03/x","repo":"app"},
	  "state":"closed",
	  "reports":[{"id":4,"from":"agent-03","to":"karmax","text":"all green","at":"2026-10-12T10:00:00Z"}],
	  "commits":["abc1234 fix the thing"],"ahead":1}`}}
	out, err := fleetctl(t, f, "result", "T1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"T1", "agent-03", "closed", "agent-03 → karmax:", "all green", "abc1234 fix the thing", "fleet/agent-03/x"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if f.reqs[0] != "GET /v1/tasks/T1 " {
		t.Errorf("request = %q", f.reqs[0])
	}
	js, err := fleetctl(t, f, "result", "T1", "--json")
	if err != nil || !strings.Contains(js, `"commits"`) {
		t.Fatalf("json = %q, %v", js, err)
	}
	if _, err := fleetctl(t, f, "result"); err == nil {
		t.Fatal("result needs a task")
	}
}

func TestWaitExitCodesSayWhy(t *testing.T) {
	for body, want := range map[string]int{
		`{"reason":"phase","phase":"down"}`: 3,
		`{"reason":"timeout"}`:              4,
	} {
		f := &fakeFleetd{reply: map[string]string{"/v1/inbox": `[]`, "/v1/wait": body}}
		_, err := fleetctl(t, f, "wait", "agent-03")
		var ec exitCode
		if !errors.As(err, &ec) || int(ec.code) != want {
			t.Errorf("%s: err = %v, want exit %d", body, err, want)
		}
	}
}

func TestWhoamiAndApi(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{
		"/v1/whoami": `{"scope":"full","name":"operator"}`,
		"/v1":        `[{"route":"GET /v1/status","scopes":["full","relay","console"]}]`,
	}}
	if out, _ := fleetctl(t, f, "whoami"); !strings.Contains(out, "operator") || !strings.Contains(out, "full") {
		t.Fatalf("whoami = %q", out)
	}
	if out, _ := fleetctl(t, f, "api"); !strings.Contains(out, "GET /v1/status") || !strings.Contains(out, "relay") {
		t.Fatalf("api = %q", out)
	}
}

func req(f *fakeFleetd, i int) string { return f.reqs[i] }

func TestPeekPromptAndArchiveGoOverHTTP(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{
		"/v1/peek":     `{"agent":"agent-03","text":"line1\nline2\n"}`,
		"/v1/archives": `{"removed":3}`,
	}}
	if out, err := fleetctl(t, f, "peek", "agent-03", "-n", "5"); err != nil || out != "line1\nline2\n" {
		t.Fatalf("peek = %q, %v", out, err)
	}
	if _, err := fleetctl(t, f, "prompt", "agent-03", "run", "the", "tests"); err != nil {
		t.Fatal(err)
	}
	out, err := fleetctl(t, f, "archive", "prune", "--older", "180d")
	if err != nil || !strings.Contains(out, "pruned 3") {
		t.Fatalf("prune = %q, %v", out, err)
	}
	want := []string{
		"GET /v1/peek?agent=agent-03&n=5 ",
		`POST /v1/prompt {"agent":"agent-03","text":"run the tests"}`,
		"DELETE /v1/archives?older=180d ",
	}
	for i, w := range want {
		if got := req(f, i); got != w {
			t.Errorf("request %d = %q, want %q", i, got, w)
		}
	}
}

func TestTasksActivityAndTodayAreHTTPClients(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{
		"/v1/tasks":    `[{"id":"T1","agent":"agent-03","assigner":"operator","assigned":"2026-10-11T10:00:00Z"}]`,
		"/v1/activity": `{"bucket_s":900,"agents":{"agent-03":[{"t":"2026-10-11T10:00:00Z","n":4}]}}`,
		"/v1/today":    `{"since":"2026-10-11T00:00:00Z","tasks_closed":2,"spend_usd":0.5,"output_tokens":10}`,
	}}
	if out, err := fleetctl(t, f, "tasks", "--open"); err != nil || !strings.Contains(out, "T1") || !strings.Contains(out, "operator") {
		t.Fatalf("tasks = %q, %v", out, err)
	}
	if out, err := fleetctl(t, f, "activity"); err != nil || !strings.Contains(out, "agent-03") || !strings.Contains(out, "(4)") {
		t.Fatalf("activity = %q, %v", out, err)
	}
	if out, err := fleetctl(t, f, "today"); err != nil || !strings.Contains(out, "2 tasks closed") || !strings.Contains(out, "$0.50") {
		t.Fatalf("today = %q, %v", out, err)
	}
	if got := req(f, 0); !strings.Contains(got, "open=1") {
		t.Errorf("tasks --open sent %q", got)
	}
	if got := req(f, 1); !strings.Contains(got, "since=24h") || !strings.Contains(got, "bucket=15m") {
		t.Errorf("activity sent %q", got)
	}
}

func TestRotateDryRunSendsTheFlagAndPrintsTheWarning(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/rotate": `{"ok":true,"dry_run":true,"closes":"T1","when":"next idle tick"}`}}
	out, err := fleetctl(t, f, "rotate", "agent-03", "--dry-run")
	if err != nil || !strings.Contains(out, "closes task T1 as rotated") || !strings.Contains(out, "dry run") {
		t.Fatalf("rotate --dry-run = %q, %v", out, err)
	}
	if got := req(f, 0); got != `POST /v1/rotate {"agent":"agent-03","dry_run":true}` {
		t.Errorf("sent %q", got)
	}
}

func TestRenderUpDownAskFleetdWhenThereIsNoFleetYAML(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/up": `{"ok":true,"output":"$ docker compose up\n"}`, "/v1/render": `{"ok":true,"output":"rendered\n"}`}}
	if out, err := fleetctl(t, f, "up", "pc2"); err != nil || out != "$ docker compose up\n" {
		t.Fatalf("up = %q, %v", out, err)
	}
	if out, err := fleetctl(t, f, "render"); err != nil || out != "rendered\n" {
		t.Fatalf("render = %q, %v", out, err)
	}
	if _, err := fleetctl(t, f, "render", "--out", "/x"); err == nil {
		t.Fatal("--out must be refused when it cannot be honoured")
	}
	if got := req(f, 0); got != `POST /v1/up {"hosts":["pc2"]}` {
		t.Errorf("up sent %q", got)
	}
}

func TestUpDryRunPrintsThePlanAndRecreatesNothing(t *testing.T) {
	dir := t.TempDir()
	state := dir + "/state"
	if err := os.MkdirAll(state+"/render/agents", 0o700); err != nil {
		t.Fatal(err)
	}
	yml := "home: /home/op\nimage: img:2\nstate_dir: " + state + "\nhosts:\n  h1: {exec: [docker]}\n" +
		"orchestrator: {host: h1, native: true}\nagents:\n  agent-01: {host: h1, token_env: T1}\n"
	_ = os.WriteFile(dir+"/fleet.yaml", []byte(yml), 0o600)
	_ = os.WriteFile(state+"/render/agents/agent-01.env", []byte("T=1\n"), 0o600)
	_ = os.WriteFile(state+"/render/compose.h1.yaml", []byte("services:\n  agent-01:\n    image: img:2\n    container_name: agent-01\n    env_file: [agents/agent-01.env]\n    environment: {FLEET_CLAUDE_MD: NEW}\n"), 0o600)
	hostYAML = dir + "/fleet.yaml"
	t.Cleanup(func() { hostYAML = "" })

	var mu sync.Mutex
	var calls []string
	old := runDocker
	defer func() { runDocker = old }()
	runDocker = func(_ context.Context, argv []string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, strings.Join(argv, " "))
		switch {
		case strings.Contains(strings.Join(argv, " "), "image inspect"):
			return []byte("sha-2\n"), nil
		case argv[1] == "inspect":
			return []byte(`[{"Image":"sha-2","Config":{"Env":["T=1","FLEET_CLAUDE_MD=OLD"],"Labels":{}}}]`), nil
		}
		return nil, errors.New("not a read-only call")
	}
	for _, flag := range []string{"--dry-run", "--plan"} {
		out, err := fleetctl(t, &fakeFleetd{}, "up", flag, "--when-idle")
		if err != nil {
			t.Fatalf("up %s: %v\n%s", flag, err, out)
		}
		if !strings.Contains(out, "agent-01 will be RECREATED — brief changed") || !strings.Contains(out, "dry run") {
			t.Errorf("up %s printed:\n%s", flag, out)
		}
	}
	for _, c := range calls {
		if strings.Contains(c, " up") || strings.Contains(c, " down") {
			t.Errorf("a dry run ran %q", c)
		}
	}
}

func TestInboxFollowSurvivesAFleetdRestartAndResumesFromTheLastID(t *testing.T) {
	var mu sync.Mutex
	var polls int
	var afters []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		polls++
		n := polls
		afters = append(afters, r.URL.Query().Get("after"))
		mu.Unlock()
		switch {
		case n == 2 || n == 3: // fleetd is restarting behind its proxy
			http.Error(w, "bad gateway", http.StatusBadGateway)
		case n == 1:
			_, _ = io.WriteString(w, `[{"id":7,"from":"agent-03","at":"2026-10-11T20:00:00Z","text":"first"}]`)
		case n == 4:
			_, _ = io.WriteString(w, `[{"id":8,"from":"agent-03","at":"2026-10-11T20:01:00Z","text":"second"}]`)
		default:
			_, _ = io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var sleeps []time.Duration
	sleep := func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		if len(sleeps) == 7 {
			cancel()
		}
		return ctx.Err()
	}
	var errOut bytes.Buffer
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := followInbox(ctx, &client{base: srv.URL, token: "tok"}, 0, false, &errOut, sleep)
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatalf("followInbox: %v (Ctrl-C is a clean exit)", err)
	}
	if !strings.Contains(string(out), "first") || !strings.Contains(string(out), "second") || strings.Count(string(out), "first") != 1 {
		t.Errorf("messages:\n%s", out)
	}
	if got := strings.Count(errOut.String(), "fleetd unreachable, retrying\n"); got != 2 {
		t.Errorf("stderr = %q, want one line per failed poll (2)", errOut.String())
	}
	if len(sleeps) < 4 || sleeps[1] != time.Second || sleeps[2] != 2*time.Second || sleeps[3] != 2*time.Second {
		t.Errorf("sleeps = %v: want poll, 1s, 2s back-off, then the poll interval again", sleeps)
	}
	mu.Lock()
	defer mu.Unlock()
	if afters[1] != "7" || afters[3] != "7" || afters[4] != "8" {
		t.Errorf("after= per poll: %v, want to resume from the last id", afters)
	}
}

func TestInboxFollowBackoffCapsAt15sAndAnExpiredTokenEnds(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	var sleeps []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	_ = followInbox(ctx, &client{base: down.URL, token: "t"}, 0, false, io.Discard, func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		if len(sleeps) == 7 {
			cancel()
		}
		return ctx.Err()
	})
	want := []time.Duration{1, 2, 4, 8, 15, 15, 15}
	for i, d := range want {
		if sleeps[i] != d*time.Second {
			t.Fatalf("sleeps = %v, want 1,2,4,8,15,15,15 seconds", sleeps)
		}
	}
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":"bad token"}`)
		}))
		err := followInbox(context.Background(), &client{base: bad.URL, token: "t"}, 0, false, io.Discard,
			func(context.Context, time.Duration) error {
				t.Error("retried after a refused token")
				return context.Canceled
			})
		bad.Close()
		if err == nil || !strings.Contains(err.Error(), "bad token") {
			t.Errorf("%d: err = %v, want the refusal", code, err)
		}
	}
}

func TestDoneKeepSessionIsSentToTheRoute(t *testing.T) {
	f := &fakeFleetd{}
	if _, err := fleetctl(t, f, "done", "agent-03", "--task", "T1", "--outcome", "superseded", "--keep-session"); err != nil {
		t.Fatal(err)
	}
	var in map[string]any
	_ = json.Unmarshal([]byte(strings.SplitN(f.reqs[0], " ", 3)[2]), &in)
	if in["keep_session"] != true || in["outcome"] != "superseded" || in["task"] != "T1" {
		t.Fatalf("request = %s", f.reqs[0])
	}
	f = &fakeFleetd{}
	_, _ = fleetctl(t, f, "done", "agent-03")
	_ = json.Unmarshal([]byte(strings.SplitN(f.reqs[0], " ", 3)[2]), &in)
	if in["keep_session"] != false {
		t.Fatalf("plain done sent %s", f.reqs[0])
	}
}

func TestImageBuildAndShipRunDockerFromFleetYAML(t *testing.T) {
	dir := t.TempDir()
	repo := dir + "/repo"
	_ = os.MkdirAll(repo+"/packaging/fleet", 0o755)
	_ = os.WriteFile(repo+"/packaging/fleet/Dockerfile", nil, 0o644)
	yml := "home: /home/op\nimage: img:3\nuid: 1000\ngid: 1000\nuser: op\nhosts:\n  h1: {exec: [docker]}\n  pc2: {exec: [docker, --context, pc2]}\n" +
		"orchestrator: {host: h1, native: true}\nagents:\n  agent-01: {host: h1, token_env: T1}\n"
	_ = os.WriteFile(dir+"/fleet.yaml", []byte(yml), 0o600)
	hostYAML = dir + "/fleet.yaml"
	t.Cleanup(func() { hostYAML = "" })

	var built, saved, loaded string
	oldRun, oldPipe := imageRun, imagePipe
	defer func() { imageRun, imagePipe = oldRun, oldPipe }()
	imageRun = func(_ context.Context, argv []string, _ io.Writer) error { built = strings.Join(argv, " "); return nil }
	imagePipe = func(_ context.Context, from, to []string, _ io.Writer) error {
		saved, loaded = strings.Join(from, " "), strings.Join(to, " ")
		return nil
	}
	out, err := fleetctl(t, &fakeFleetd{}, "image", "build", "--tag", "img:4", "--src", repo)
	if err != nil || !strings.HasPrefix(out, "$ docker build ") || !strings.Contains(built, "-t img:4") ||
		!strings.Contains(built, "HOME_DIR=/home/op") || !strings.Contains(built, "UID=1000") {
		t.Fatalf("build = %q, ran %q, %v", out, built, err)
	}
	out, err = fleetctl(t, &fakeFleetd{}, "image", "ship", "pc2")
	if err != nil || saved != "docker save img:3" || loaded != "docker --context pc2 load" || !strings.HasPrefix(out, "$ docker save img:3 | ") {
		t.Fatalf("ship = %q, %q | %q, %v", out, saved, loaded, err)
	}
	if _, err := fleetctl(t, &fakeFleetd{}, "image", "ship"); err == nil {
		t.Error("ship needs a host")
	}
}

// flaky answers /v1/wait: the first n requests lose their connection (fleetd
// restarting), then it replies with reply, or with an HTTP status if code != 0.
type flaky struct {
	mu    sync.Mutex
	drops int
	paths []string
	code  int
	reply string
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	drop := r.URL.Path == "/v1/wait" && f.drops > 0
	if drop {
		f.drops--
	}
	f.mu.Unlock()
	if drop {
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
		return
	}
	if r.URL.Path == "/v1/inbox" {
		_, _ = io.WriteString(w, `[{"ID":7}]`)
		return
	}
	if f.code != 0 {
		w.WriteHeader(f.code)
	}
	_, _ = io.WriteString(w, f.reply)
}

func waitWith(t *testing.T, f *flaky, task string, timeout time.Duration) (string, []time.Duration, error) {
	t.Helper()
	srv := httptest.NewServer(f)
	defer srv.Close()
	var errOut bytes.Buffer
	var slept []time.Duration
	_, err := waitFor(context.Background(), &client{base: srv.URL, token: "t"}, "agent-03", task, -1, timeout, &errOut,
		func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil })
	return errOut.String(), slept, err
}

func TestWaitRetriesWhenFleetdRestartsMidWait(t *testing.T) {
	f := &flaky{drops: 3, reply: `{"Reason":"message","Message":{"ID":8,"From":"agent-03","Text":"ready"}}`}
	msg, slept, err := waitWith(t, f, "", time.Hour)
	// (net/http itself retries a dropped GET once, unseen: 3 drops are 1-2 retries.)
	if err != nil || !strings.Contains(msg, "fleetd unreachable, retrying") {
		t.Fatalf("err %v, stderr %q", err, msg)
	}
	if len(slept) < 1 || slept[0] != time.Second || (len(slept) > 1 && slept[1] != 2*time.Second) {
		t.Errorf("backoff = %v", slept)
	}
}

func TestWaitGivesUpAtItsTimeoutNotAtTheFirstFailure(t *testing.T) {
	f := &flaky{drops: 1 << 20}
	_, _, err := waitWith(t, f, "", 3*time.Second) // sleeps are fake: only the clock's headroom counts
	if err != nil {
		t.Fatalf("a wait that ran out of time is a timeout result, not an error: %v", err)
	}
}

func TestWaitOnAMissingTaskFailsFastWithoutRetrying(t *testing.T) {
	f := &flaky{code: 404, reply: `{"error":"no task T9"}`}
	msg, _, err := waitWith(t, f, "T9", time.Hour)
	if err == nil || err.Error() != "no task T9" || msg != "" {
		t.Fatalf("err %v, stderr %q", err, msg)
	}
	n := 0
	for _, p := range f.paths {
		if p == "/v1/wait" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("asked %d times", n)
	}
}

func TestGuidePrintsTheGuide(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/guide": `{"text":"the guide\nline two"}`}}
	out, err := fleetctl(t, f, "guide")
	if err != nil || out != "the guide\nline two\n" {
		t.Fatalf("out = %q, %v", out, err)
	}
}

func TestSkillInstallWritesTheSkill(t *testing.T) {
	dir := t.TempDir()
	out, err := fleetctl(t, &fakeFleetd{}, "skill", "install", "--dir", dir)
	p := dir + "/fleet/SKILL.md"
	if err != nil || strings.TrimSpace(out) != p {
		t.Fatalf("out = %q, %v", out, err)
	}
	b, _ := os.ReadFile(p)
	st, _ := os.Stat(p)
	if !strings.HasPrefix(string(b), "---\nname: fleet\n") || st.Mode().Perm() != 0o644 {
		t.Fatalf("skill file %v starts %q", st.Mode(), b[:20])
	}
	if _, err := fleetctl(t, &fakeFleetd{}, "skill", "install", "--dir", dir); err != nil {
		t.Fatalf("reinstall over our own skill: %v", err)
	}
}

func TestSkillInstallRefusesAnUnrelatedFile(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/fleet/SKILL.md"
	_ = os.MkdirAll(dir+"/fleet", 0o755)
	_ = os.WriteFile(p, []byte("my own notes\n"), 0o644)
	if _, err := fleetctl(t, &fakeFleetd{}, "skill", "install", "--dir", dir); err == nil {
		t.Fatal("overwrote a file that is not a skill")
	}
	if b, _ := os.ReadFile(p); string(b) != "my own notes\n" {
		t.Fatalf("file changed: %q", b)
	}
	if _, err := fleetctl(t, &fakeFleetd{}, "skill", "install", "--dir", dir, "--force"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !strings.HasPrefix(string(b), "---\n") {
		t.Fatalf("--force did not write: %q", b)
	}
}

func TestSkillInstallPrintAndDefaultDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	out, err := fleetctl(t, &fakeFleetd{}, "skill", "install", "--print")
	if err != nil || !strings.HasPrefix(out, "---\nname: fleet\n") {
		t.Fatalf("--print = %q, %v", out, err)
	}
	if _, err := os.Stat(home + "/.claude"); err == nil {
		t.Fatal("--print wrote files")
	}
	if _, err := fleetctl(t, &fakeFleetd{}, "skill", "install"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home + "/.claude/skills/fleet/SKILL.md"); err != nil {
		t.Fatal(err)
	}
}

func reqBody(t *testing.T, req string) map[string]any {
	t.Helper()
	var in map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(req, " ", 3)[2]), &in); err != nil {
		t.Fatalf("request %q: %v", req, err)
	}
	return in
}

func TestDoneByTaskSendsNoAgent(t *testing.T) {
	f := &fakeFleetd{}
	if _, err := fleetctl(t, f, "done", "--task", "T1"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.reqs[0], "POST /v1/done ") {
		t.Fatalf("request = %s", f.reqs[0])
	}
	if in := reqBody(t, f.reqs[0]); in["agent"] != "" || in["task"] != "T1" {
		t.Fatalf("request = %s", f.reqs[0])
	}
	if _, err := fleetctl(t, &fakeFleetd{}, "done"); err == nil {
		t.Fatal("done with neither agent nor --task must fail")
	}
}

// Repeated --task flags used to keep only the last one while printing "ok", so the
// other tasks stayed open and held their agents.
func TestDoneClosesEveryRepeatedTask(t *testing.T) {
	f := &fakeFleetd{}
	out, err := fleetctl(t, f, "done", "--task", "T1", "--task", "T2", "--task", "T3", "--outcome", "orphaned")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 3 {
		t.Fatalf("%d requests, want one per task: %q", len(f.reqs), f.reqs)
	}
	for i, id := range []string{"T1", "T2", "T3"} {
		if in := reqBody(t, f.reqs[i]); in["task"] != id || in["agent"] != "" || in["outcome"] != "orphaned" {
			t.Errorf("request %d = %s", i, f.reqs[i])
		}
		if !strings.Contains(out, "ok "+id) {
			t.Errorf("output %q does not confirm %s", out, id)
		}
	}
	for _, args := range [][]string{
		{"done", "agent-03", "--task", "T1", "--task", "T2"},
		{"done", "--task", "T1", "--task", "T2", "--branch", "b"},
		{"done", "--task", "T1", "--task", "T2", "--pr", "u"},
	} {
		f := &fakeFleetd{}
		if _, err := fleetctl(t, f, args...); err == nil || len(f.reqs) != 0 {
			t.Errorf("%q: err = %v, %d requests; want a refusal before any request", args, err, len(f.reqs))
		}
	}
}

func TestFollowupAndCancelCommands(t *testing.T) {
	f := &fakeFleetd{}
	if _, err := fleetctl(t, f, "followup", "T1", "also", "the empty case", "--from", "karmax"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.reqs[0], "POST /v1/tasks/T1/followup ") {
		t.Fatalf("request = %s", f.reqs[0])
	}
	if in := reqBody(t, f.reqs[0]); in["text"] != "also the empty case" || in["from"] != "karmax" {
		t.Fatalf("request = %s", f.reqs[0])
	}
	if _, err := fleetctl(t, f, "cancel", "T1", "--reason", "wrong repo"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.reqs[1], "POST /v1/tasks/T1/cancel ") {
		t.Fatalf("request = %s", f.reqs[1])
	}
	if in := reqBody(t, f.reqs[1]); in["reason"] != "wrong repo" {
		t.Fatalf("request = %s", f.reqs[1])
	}
	if _, err := fleetctl(t, f, "followup", "T1"); err == nil {
		t.Fatal("followup without text must fail")
	}
	if _, err := fleetctl(t, f, "cancel"); err == nil {
		t.Fatal("cancel without a task must fail")
	}
}
