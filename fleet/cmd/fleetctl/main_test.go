package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
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

// fleetctl runs one command against a fake fleetd and returns its stdout.
func fleetctl(t *testing.T, f *fakeFleetd, args ...string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("FLEETD_URL", srv.URL)
	t.Setenv("FLEET_TOKEN", "tok")
	t.Setenv("FLEET_NAME", "")
	t.Setenv("FLEET_AGENT", "")
	t.Setenv("FLEET_CONFIG", t.TempDir()+"/none.yaml") // not on a fleet host: everything goes through fleetd
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
	out, err := fleetctl(t, f, "wait", "agent-03", "--task", "T1", "--timeout", "10m")
	if err != nil || !strings.Contains(out, "ready") {
		t.Fatalf("out = %q, %v", out, err)
	}
	if got := f.reqs[1]; !strings.HasPrefix(got, "GET /v1/wait?") || !strings.Contains(got, "after=9") ||
		!strings.Contains(got, "task=T1") || !strings.Contains(got, "timeout=10m") {
		t.Fatalf("request = %s", got)
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
