package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// serve answers fleetd's routes from a table and records each request.
type serve struct {
	mu   sync.Mutex
	reqs []string
	body []string
	code map[string]int
	out  map[string]string
}

func (s *serve) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Method+" "+r.URL.RequestURI())
	s.body = append(s.body, string(b))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if c := s.code[r.URL.Path]; c != 0 {
		w.WriteHeader(c)
	}
	_, _ = io.WriteString(w, s.out[r.URL.Path])
}

// ctl runs fleetctl against s with the given stdin, returning stdout.
func ctl(t *testing.T, s *serve, stdin string, args ...string) (string, error) {
	t.Helper()
	ctlEnv(t, s)
	return ctlRunIn(stdin, args...)
}

// ctlEnv serves s and points fleetctl at it, as nobody in particular: no
// agent, session or Claude Code session name in its environment.
func ctlEnv(t *testing.T, s *serve) {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	t.Setenv("FLEETD_URL", srv.URL)
	t.Setenv("FLEET_TOKEN", "tok")
	t.Setenv("FLEET_NAME", "")
	t.Setenv("FLEET_AGENT", "")
	t.Setenv("FLEET_SESSION", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("FLEET_CONFIG", t.TempDir()+"/none.yaml")
}

// ctlRun runs fleetctl against the server ctlEnv made.
func ctlRun(args ...string) (string, error) { return ctlRunIn("", args...) }

func ctlRunIn(stdin string, args ...string) (string, error) {
	inR, inW, _ := os.Pipe()
	go func() { _, _ = io.WriteString(inW, stdin); inW.Close() }()
	r, w, _ := os.Pipe()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	err := run(args[0], args[1:])
	w.Close()
	os.Stdin, os.Stdout = oldIn, oldOut
	return <-done, err
}

func exitOf(err error) int {
	var ec exitCode
	if errors.As(err, &ec) {
		return ec.code
	}
	return -1
}

const runReply = `{"task":"20261012-fix-it-ab12","agent":"agent-02","why":"5h 3%, 7d 10%, idle 4m"}`

func sentBrief(t *testing.T, s *serve) map[string]string {
	t.Helper()
	var in map[string]string
	if err := json.Unmarshal([]byte(s.body[0]), &in); err != nil {
		t.Fatalf("body %q: %v", s.body[0], err)
	}
	return in
}

func TestRunSendsTheBriefAndPrintsWhereItWent(t *testing.T) {
	s := &serve{out: map[string]string{"/v1/run": runReply}}
	out, err := ctl(t, s, "", "run", "--repo", "KARMAX", "--queue", "30m", "--from", "karmax", "fix", "the", "bug")
	if err != nil || strings.TrimSpace(out) != "task 20261012-fix-it-ab12 → agent-02 (5h 3%, 7d 10%, idle 4m)" {
		t.Fatalf("out = %q, %v", out, err)
	}
	in := sentBrief(t, s)
	if s.reqs[0] != "POST /v1/run" || in["brief"] != "fix the bug" || in["repo"] != "KARMAX" || in["queue"] != "30m0s" || in["from"] != "karmax" {
		t.Errorf("request = %s %v", s.reqs[0], in)
	}
}

func TestRunBriefFromStdinArrivesByteForByte(t *testing.T) {
	brief := "line one\n  $(rm -rf /) `id` 'q' \"d\" ; & | > <\n\ttabbed\\n\n\n"
	s := &serve{out: map[string]string{"/v1/run": runReply}}
	if _, err := ctl(t, s, brief, "run", "--repo", "r", "-"); err != nil {
		t.Fatal(err)
	}
	if got := sentBrief(t, s)["brief"]; got != brief {
		t.Errorf("brief = %q, want %q", got, brief)
	}
}

func TestRunBriefFromFile(t *testing.T) {
	path := t.TempDir() + "/brief.md"
	body := "# Task\n$HOME `x`\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &serve{out: map[string]string{"/v1/run": runReply}}
	if _, err := ctl(t, s, "", "run", "--brief-file", path); err != nil {
		t.Fatal(err)
	}
	if got := sentBrief(t, s)["brief"]; got != body {
		t.Errorf("brief = %q", got)
	}
}

func TestRunNeedsABrief(t *testing.T) {
	s := &serve{}
	if _, err := ctl(t, s, "", "run", "--repo", "r"); err == nil || len(s.reqs) != 0 {
		t.Errorf("err = %v, requests = %v", err, s.reqs)
	}
}

func TestRunJSONPrintsTheReply(t *testing.T) {
	s := &serve{out: map[string]string{"/v1/run": runReply}}
	out, err := ctl(t, s, "", "run", "--json", "go")
	var rep struct{ Task, Agent string }
	if err != nil || json.Unmarshal([]byte(out), &rep) != nil || rep.Agent != "agent-02" {
		t.Errorf("out = %q, %v", out, err)
	}
}

func TestRunNoFreeAgentPrintsCandidatesAndExits5(t *testing.T) {
	s := &serve{code: map[string]int{"/v1/run": 409}, out: map[string]string{"/v1/run": `{"error":"no free agent (…)","candidates":[
		{"agent":"agent-01","eligible":false,"why":"working on T1","five_hour_pct":40,"seven_day_pct":12,"idle_s":0},
		{"agent":"agent-02","eligible":false,"why":"down"}]}`}}
	out, err := ctl(t, s, "", "run", "go")
	if exitOf(err) != 5 || !strings.Contains(out, "agent-01") || !strings.Contains(out, "working on T1") || !strings.Contains(out, "down") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestRunNamedAgentBusyExits5WithTheReason(t *testing.T) {
	s := &serve{code: map[string]int{"/v1/run": 409}, out: map[string]string{"/v1/run": `{"error":"conflict: agent-02 is not free: working"}`}}
	_, err := ctl(t, s, "", "run", "--agent", "agent-02", "go")
	if exitOf(err) != 5 {
		t.Errorf("err = %v", err)
	}
}

func TestRunOtherErrorsExit1(t *testing.T) {
	s := &serve{code: map[string]int{"/v1/run": 400}, out: map[string]string{"/v1/run": `{"error":"bad"}`}}
	_, err := ctl(t, s, "", "run", "go")
	if err == nil || exitOf(err) != -1 || !strings.Contains(err.Error(), "bad") {
		t.Errorf("err = %v", err)
	}
}

func TestRunWaitPrintsTheReport(t *testing.T) {
	s := &serve{out: map[string]string{"/v1/run": runReply,
		"/v1/wait": `{"reason":"message","message":{"id":3,"from":"agent-02","text":"ready on fleet/agent-02/x"}}`}}
	out, err := ctl(t, s, "", "run", "--wait", "--timeout", "10m", "go")
	if err != nil || !strings.Contains(out, "ready on fleet/agent-02/x") || !strings.Contains(out, "task 20261012-fix-it-ab12") {
		t.Fatalf("out = %q, %v", out, err)
	}
	u, _ := url.Parse(s.reqs[len(s.reqs)-1][len("GET "):])
	q := u.Query()
	if u.Path != "/v1/wait" || q.Get("agent") != "agent-02" || q.Get("task") != "20261012-fix-it-ab12" || q.Get("timeout") != "10m0s" && q.Get("timeout") != "10m" {
		t.Errorf("wait request = %v", s.reqs)
	}
}

func TestRunWaitExitCodesAsWait(t *testing.T) {
	for body, want := range map[string]int{`{"reason":"phase","phase":"down"}`: 3, `{"reason":"timeout"}`: 4, `{"reason":"closed","task":{"id":"T","outcome":"done"}}`: 0} {
		s := &serve{out: map[string]string{"/v1/run": runReply, "/v1/wait": body}}
		_, err := ctl(t, s, "", "run", "--wait", "go")
		if got := exitOf(err); got != want && !(want == 0 && err == nil) {
			t.Errorf("%s: err = %v, want exit %d", body, err, want)
		}
	}
}

func TestPickPrintsTheTableAndExits5WhenNoneEligible(t *testing.T) {
	two := `{"agent":"agent-02","why":"5h 3%","candidates":[
		{"agent":"agent-02","eligible":true,"why":"5h 3%","five_hour_pct":3,"seven_day_pct":10,"idle_s":240},
		{"agent":"agent-01","eligible":false,"why":"down"}]}`
	s := &serve{out: map[string]string{"/v1/pick": two}}
	out, err := ctl(t, s, "", "pick", "--repo", "KARMAX")
	if err != nil || s.reqs[0] != "GET /v1/pick?repo=KARMAX" {
		t.Fatalf("err = %v, req = %v", err, s.reqs)
	}
	if i, j := strings.Index(out, "agent-02"), strings.Index(out, "agent-01"); i < 0 || j < i || !strings.Contains(out, "down") {
		t.Errorf("chosen first: %q", out)
	}
	none := `{"agent":"","why":"","candidates":[{"agent":"agent-01","eligible":false,"why":"down"}]}`
	s = &serve{out: map[string]string{"/v1/pick": none}}
	out, err = ctl(t, s, "", "pick")
	if exitOf(err) != 5 || !strings.Contains(out, "down") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

// fleetd restarting (a deploy, a power cut) as a run goes in: fleetctl tries
// again, with a task id of its own making, so a run that did land is answered
// with its agent, never given to a second one.
func TestRunRetriesWithOneTaskIDWhileFleetdIsAway(t *testing.T) {
	old := runRetrySleep
	runRetrySleep = func(time.Duration) {}
	defer func() { runRetrySleep = old }()
	s := &serve{out: map[string]string{"/v1/run": runReply}}
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		s.ServeHTTP(w, r)
	}))
	defer srv.Close()
	t.Setenv("FLEETD_URL", srv.URL)
	t.Setenv("FLEET_TOKEN", "tok")
	t.Setenv("FLEET_NAME", "")
	t.Setenv("FLEET_AGENT", "")
	t.Setenv("FLEET_SESSION", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("FLEET_CONFIG", t.TempDir()+"/none.yaml")
	var sent []string
	srvBodies := func() []string { s.mu.Lock(); defer s.mu.Unlock(); return s.body }
	if err := run("run", []string{"--json", "fix", "the", "login", "redirect"}); err != nil {
		t.Fatal(err)
	}
	sent = srvBodies()
	if n.Load() != 3 || len(sent) != 1 {
		t.Fatalf("tries %d, bodies %q", n.Load(), sent)
	}
	var in map[string]string
	_ = json.Unmarshal([]byte(sent[0]), &in)
	if !regexp.MustCompile(`^\d{8}-fix-the-login-redirect-[0-9a-f]{4}$`).MatchString(in["task"]) {
		t.Fatalf("task id %q: fleetctl makes it, so every try names the same run", in["task"])
	}
}

// done is the same request however often it is sent: while fleetd is away,
// fleetctl sends it again rather than leave the task open.
func TestDoneIsSentAgainWhileFleetdIsAway(t *testing.T) {
	old := runRetrySleep
	runRetrySleep = func(time.Duration) {}
	defer func() { runRetrySleep = old }()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	t.Setenv("FLEETD_URL", srv.URL)
	t.Setenv("FLEET_TOKEN", "tok")
	t.Setenv("FLEET_CONFIG", t.TempDir()+"/none.yaml")
	if err := run("done", []string{"--task", "T1"}); err != nil || n.Load() != 3 {
		t.Fatalf("err %v after %d tries", err, n.Load())
	}
	n.Store(0)
	if err := run("done", []string{"--task", "T1", "--task", "T2"}); err != nil || n.Load() != 4 {
		t.Fatalf("several: err %v after %d tries", err, n.Load())
	}
}
