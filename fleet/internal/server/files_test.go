package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/filexfer"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

func (f *fake) Brief(_ context.Context, task, from, repo, brief string, _ time.Time) error {
	f.calls = append(f.calls, fmt.Sprintf("brief %s from=%q repo=%q %s", task, from, repo, brief))
	return nil
}

func (f *fake) PutFiles(_ context.Context, agent, dir string, body io.Reader) (int64, error) {
	b, _ := io.ReadAll(body)
	f.calls = append(f.calls, fmt.Sprintf("put %s %s %s", agent, dir, b))
	if agent == "agent-big" {
		return int64(len(b)), fmt.Errorf("%w (2 GiB)", filexfer.ErrTooLarge)
	}
	return int64(len(b)), nil
}

func (f *fake) GetFiles(_ context.Context, agent, path string, w io.Writer) error {
	f.calls = append(f.calls, "get "+agent+" "+path)
	switch path {
	case "/etc":
		return fmt.Errorf("%w: files go under /work or /tmp only", daemon.ErrBadInput)
	case "/work/half":
		_, _ = io.WriteString(w, "PART")
		return fmt.Errorf("the container went away")
	}
	_, err := io.WriteString(w, "TARDATA")
	return err
}

func (f *fake) Rsh(ctx context.Context, agent string, argv []string, stdin io.Reader, stdout io.Writer) (target.Result, error) {
	f.calls = append(f.calls, fmt.Sprintf("rsh %s %q", agent, argv))
	if agent == "agent-real" { // a real command, as the exec target runs it
		return target.Exec{}.Stream(ctx, argv, stdin, stdout)
	}
	_, err := io.Copy(stdout, stdin)
	return target.Result{Exit: 3, Stderr: "oops"}, err
}

func TestFileRoutesAreForFullAndMembersOnly(t *testing.T) {
	_, h, _ := tailnetSetup(t)
	_, api := setup(t)
	for _, c := range []struct {
		method, path, token, ip string
		want                    int
	}{
		{"PUT", "/v1/agents/agent-03/files", "full", "", 200},
		{"PUT", "/v1/agents/agent-03/files", "relay", "", 403},
		{"PUT", "/v1/agents/agent-03/files", "console", "", 403},
		{"PUT", "/v1/agents/agent-03/files", "", "", 401},
		{"GET", "/v1/agents/agent-03/files?path=/work/x", "relay", "", 403},
		{"GET", "/v1/agents/agent-03/files?path=/work/x", "console", "", 403},
		{"POST", "/v1/agents/agent-03/rsh?arg=true", "relay", "", 403},
		{"POST", "/v1/agents/agent-03/rsh?arg=true", "console", "", 403},
		// Ram's task T-ram is on agent-03.
		{"PUT", "/v1/agents/agent-03/files", "", ramIP, 200},
		{"GET", "/v1/agents/agent-03/files?path=/work/x", "", ramIP, 200},
		{"PUT", "/v1/agents/agent-02/files", "", ramIP, 403}, // on the operator's task T1
		{"GET", "/v1/agents/agent-02/files?path=/work/x", "", ramIP, 403},
		{"PUT", "/v1/agents/agent-05/files", "", ramIP, 403}, // on no task
		{"PUT", "/v1/agents/agent-02/files", "", opIP, 200},
		{"POST", "/v1/agents/agent-03/rsh?arg=true", "", ramIP, 403}, // rsh is full scope
		{"POST", "/v1/agents/agent-03/rsh?arg=true", "", opIP, 200},
	} {
		var rec *httptest.ResponseRecorder
		if c.ip != "" {
			rec = tdo(t, h, c.method, c.path, c.ip, "", "tar")
		} else {
			rec = do(t, api, c.method, c.path, c.token, "tar")
		}
		if rec.Code != c.want {
			t.Errorf("%s %s as %q%s: %d (%s), want %d", c.method, c.path, c.token, c.ip, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
	}
}

func TestAMemberRefusedAnAgentNeverReachesTheFleet(t *testing.T) {
	f, h, _ := tailnetSetup(t)
	tdo(t, h, "PUT", "/v1/agents/agent-02/files?path=/work/x", ramIP, "", "tar")
	tdo(t, h, "GET", "/v1/agents/agent-02/files?path=/work/x", ramIP, "", "")
	if len(f.calls) != 0 {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestAnUploadStreamsTheBodyToTheFleetWithItsPath(t *testing.T) {
	f, h := setup(t)
	rec := do(t, h, "PUT", "/v1/agents/agent-03/files?path=/work/inbox/T9", "full", "TARBYTES")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"bytes":8`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := f.calls[len(f.calls)-1]; got != "put agent-03 /work/inbox/T9 TARBYTES" {
		t.Errorf("call = %q", got)
	}
}

func TestAnUploadOverTheCapIs413AndARefusedPathIs400(t *testing.T) {
	_, h := setup(t)
	if rec := do(t, h, "PUT", "/v1/agents/agent-big/files", "full", "x"); rec.Code != 413 || !strings.Contains(rec.Body.String(), "size limit") {
		t.Errorf("over the cap: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/v1/agents/agent-03/files?path=/etc", "full", ""); rec.Code != 400 || !strings.Contains(rec.Body.String(), "/work or /tmp") {
		t.Errorf("refused path: %d %s", rec.Code, rec.Body)
	}
}

func TestADownloadIsTheTarAndAStreamThatBreaksSaysWhyInATrailer(t *testing.T) {
	_, h := setup(t)
	rec := do(t, h, "GET", "/v1/agents/agent-03/files?path=/work/x", "full", "")
	if rec.Code != 200 || rec.Body.String() != "TARDATA" || rec.Header().Get("Content-Type") != "application/x-tar" {
		t.Fatalf("%d %q %s", rec.Code, rec.Body, rec.Header().Get("Content-Type"))
	}
	rec = do(t, h, "GET", "/v1/agents/agent-03/files?path=/work/half", "full", "")
	if rec.Code != 200 || rec.Body.String() != "PART" {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
	if got := rec.Result().Trailer.Get("X-Fleet-Error"); !strings.Contains(got, "container went away") {
		t.Errorf("trailer = %q (%v)", got, rec.Result().Trailer)
	}
}

// rsh reads the request while it writes the response, and ends with the
// command's exit status in a trailer.
func TestRshStreamsBothWaysAndReportsTheExit(t *testing.T) {
	f, h := setup(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	pr, pw := io.Pipe()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/agents/agent-03/rsh?arg=rsync&arg=--server", pr)
	req.Header.Set("Authorization", "Bearer full")
	respc := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
		}
		respc <- resp
	}()
	_, _ = io.WriteString(pw, "ping\n")
	resp := <-respc
	if resp == nil {
		t.FailNow()
	}
	defer resp.Body.Close()
	buf := make([]byte, 5)
	// The echo arrives while the request is still open: both ways at once.
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "ping\n" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	pw.Close()
	rest, _ := io.ReadAll(resp.Body)
	if len(rest) != 0 || resp.Trailer.Get("X-Fleet-Exit") != "3" || resp.Trailer.Get("X-Fleet-Stderr") != "oops" {
		t.Errorf("rest %q, trailers %v", rest, resp.Trailer)
	}
	if got := f.calls[len(f.calls)-1]; got != `rsh agent-03 ["rsync" "--server"]` {
		t.Errorf("call = %q", got)
	}
}

func TestABriefIsAMemberOwnTaskOnly(t *testing.T) {
	f, h, _ := tailnetSetup(t)
	if rec := tdo(t, h, "POST", "/v1/tasks/T-ram/brief", ramIP, "", `{"brief":"go"}`); rec.Code != 200 {
		t.Fatalf("own task: %d %s", rec.Code, rec.Body)
	}
	if rec := tdo(t, h, "POST", "/v1/tasks/T1/brief", ramIP, "", `{"brief":"go"}`); rec.Code != 403 {
		t.Errorf("someone else's task: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/v1/tasks/T-ram/brief", "relay", `{"brief":"go"}`); rec.Code != 403 {
		t.Errorf("an agent: %d", rec.Code)
	}
	if got := f.calls[len(f.calls)-1]; !strings.Contains(got, `brief T-ram from="ram@example.com"`) {
		t.Errorf("call = %q", got)
	}
}

func TestAWorkerNameWithItsSlotReachesTheFilesRoute(t *testing.T) {
	f, h := setup(t)
	if rec := do(t, h, "PUT", "/v1/agents/agent-03:2/files?path=/work/inbox/T9", "full", "X"); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := f.calls[len(f.calls)-1]; got != "put agent-03:2 /work/inbox/T9 X" {
		t.Errorf("call = %q", got)
	}
}

// The command ends while the caller's request is still open: the response ends
// with its exit status, and nothing waits for the caller to close its side.
func TestRshEndsWithTheCommandWhileTheRequestBodyIsStillOpen(t *testing.T) {
	_, h := setup(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	pr, pw := io.Pipe()
	defer pw.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/agents/agent-real/rsh?arg=sh&arg=-c&arg=echo+bye%3B+exit+3", pr)
	req.Header.Set("Authorization", "Bearer full")
	type result struct {
		body    string
		trailer http.Header
		err     error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		done <- result{string(b), resp.Trailer, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.body != "bye\n" || r.trailer.Get("X-Fleet-Exit") != "3" {
			t.Errorf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the response waited for the request body to close")
	}
}
