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
