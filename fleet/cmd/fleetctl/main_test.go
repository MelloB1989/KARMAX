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

// fleetctl runs one command against a fake fleetd and returns its stdout.
func fleetctl(t *testing.T, f *fakeFleetd, args ...string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("FLEETD_URL", srv.URL)
	t.Setenv("FLEET_TOKEN", "tok")
	t.Setenv("FLEET_NAME", "")
	t.Setenv("FLEET_AGENT", "")
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
	t.Setenv("FLEET_CONFIG", dir+"/fleet.yaml")

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
