package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
)

var fakeCandidates = []daemon.Candidate{
	{Agent: "agent-01", Why: "working on T1"},
	{Agent: "agent-02", Why: "down"},
}

func (f *fake) Pick(repo string, _ time.Time) daemon.PickReply {
	f.calls = append(f.calls, "pick "+repo)
	return daemon.PickReply{Agent: "agent-02", Why: "5h 0%", Candidates: []daemon.Candidate{{Agent: "agent-02", Eligible: true}}}
}

func (f *fake) Run(_ context.Context, in daemon.RunRequest, _ time.Time) (daemon.RunReply, error) {
	call := fmt.Sprintf("run brief=%q repo=%q agent=%q from=%q task=%q queue=%s", in.Brief, in.Repo, in.Agent, in.From, in.Task, in.Queue)
	if in.Limit > 0 {
		call += fmt.Sprintf(" limit=%d", in.Limit)
	}
	if in.Model != "" {
		call += " model=" + in.Model
	}
	if len(in.Among) > 0 {
		call += " among=" + strings.Join(in.Among, ",")
	}
	f.calls, f.runs = append(f.calls, call), append(f.runs, in)
	switch {
	case in.Brief == "nobody free":
		return daemon.RunReply{}, &daemon.NoFreeAgent{Candidates: fakeCandidates}
	case in.Agent == "busy":
		return daemon.RunReply{}, fmt.Errorf("%w: busy is not free: working", daemon.ErrConflict)
	case in.Repo == "bad":
		return daemon.RunReply{}, fmt.Errorf("%w: repo", daemon.ErrBadInput)
	case in.Brief == "boom":
		return daemon.RunReply{}, errors.New("boom")
	}
	return daemon.RunReply{Task: "T-new", Agent: "agent-02", Why: "5h 0%"}, nil
}

func TestRunAndPickScopes(t *testing.T) {
	_, h := setup(t)
	for _, c := range []struct {
		method, path, token, body string
		want                      int
	}{
		{"POST", "/v1/run", "full", `{"brief":"x"}`, 200},
		{"POST", "/v1/run", "console", `{"brief":"x"}`, 403},
		{"POST", "/v1/run", "relay", `{"brief":"x"}`, 403},
		{"POST", "/v1/run", "", `{"brief":"x"}`, 401},
		{"GET", "/v1/pick", "full", "", 200},
		{"GET", "/v1/pick?repo=KARMAX", "console", "", 200},
		{"GET", "/v1/pick", "relay", "", 403},
	} {
		if rec := do(t, h, c.method, c.path, c.token, c.body); rec.Code != c.want {
			t.Errorf("%s %s as %q: %d (%s), want %d", c.method, c.path, c.token, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
	}
}

func TestModelRouteScopes(t *testing.T) {
	f, h := setup(t)
	for _, c := range []struct {
		method, path, token, body string
		want                      int
	}{
		{"GET", "/v1/agents/agent-03/model", "full", "", 200},
		{"GET", "/v1/agents/agent-03/model", "console", "", 200},
		{"GET", "/v1/agents/agent-03/model", "relay", "", 403},
		{"POST", "/v1/agents/agent-03/model", "full", `{"model":"opus"}`, 200},
		{"POST", "/v1/agents/agent-03/model", "full", `{"reset":true}`, 200},
		{"POST", "/v1/agents/agent-03/model", "full", `{}`, 400},
		{"POST", "/v1/agents/agent-03/model", "full", `{"model":"opus","reset":true}`, 400},
		{"POST", "/v1/agents/agent-03/model", "console", `{"model":"opus"}`, 403},
		{"POST", "/v1/agents/agent-03/model", "relay", `{"model":"opus"}`, 403},
		{"POST", "/v1/agents/agent-03/model", "", `{"model":"opus"}`, 401},
	} {
		if rec := do(t, h, c.method, c.path, c.token, c.body); rec.Code != c.want {
			t.Errorf("%s %s as %q: %d (%s), want %d", c.method, c.path, c.token, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
	}
	do(t, h, "POST", "/v1/run", "full", `{"brief":"x","model":"opus"}`)
	if got := strings.Join(f.calls, "|"); !strings.Contains(got, " model=opus") {
		t.Errorf("run did not carry the model: %s", got)
	}
	if got := strings.Join(f.calls, "|"); !strings.Contains(got, "setmodel agent-03 opus") || !strings.Contains(got, "setmodel agent-03 |") {
		t.Errorf("calls = %s", got)
	}
}

func TestRunPassesTheRequestAndReturnsTheReply(t *testing.T) {
	f, h := setup(t)
	rec := do(t, h, "POST", "/v1/run", "full", `{"brief":"do it","repo":"KARMAX","agent":"agent-02","from":"karmax","task":"T9","queue":"30m"}`)
	var out daemon.RunReply
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Task != "T-new" || out.Agent != "agent-02" || out.Why == "" {
		t.Fatalf("body = %s", rec.Body)
	}
	want := `run brief="do it" repo="KARMAX" agent="agent-02" from="karmax" task="T9" queue=30m0s`
	if got := f.calls[len(f.calls)-1]; got != want {
		t.Errorf("call = %s", got)
	}
	do(t, h, "POST", "/v1/run", "full", `{"brief":"x"}`)
	if got := f.calls[len(f.calls)-1]; !strings.Contains(got, `from=""`) {
		t.Errorf("the daemon defaults from: %s", got)
	}
}

func TestRunNoFreeAgentIs409WithTheCandidates(t *testing.T) {
	_, h := setup(t)
	rec := do(t, h, "POST", "/v1/run", "full", `{"brief":"nobody free"}`)
	var out struct {
		Error      string             `json:"error"`
		Candidates []daemon.Candidate `json:"candidates"`
	}
	if rec.Code != 409 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Candidates) != 2 ||
		out.Candidates[0].Agent != "agent-01" || !strings.Contains(out.Error, "no free agent") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "POST", "/v1/run", "full", `{"brief":"x","agent":"busy"}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "busy is not free") || strings.Contains(rec.Body.String(), "candidates") {
		t.Errorf("busy agent: %d %s", rec.Code, rec.Body)
	}
}

func TestRunBadInputIs400(t *testing.T) {
	f, h := setup(t)
	for _, body := range []string{`{}`, `{"brief":"  "}`, `not json`, `{"brief":"x","queue":"soon"}`, `{"brief":"x","queue":"-5m"}`, `{"brief":"x","repo":"bad"}`} {
		if rec := do(t, h, "POST", "/v1/run", "full", body); rec.Code != 400 {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	for _, c := range f.calls {
		if strings.Contains(c, "soon") || strings.Contains(c, "queue=-") {
			t.Errorf("a bad queue reached the fleet: %s", c)
		}
	}
	if rec := do(t, h, "POST", "/v1/run", "full", `{"brief":"boom"}`); rec.Code != 500 {
		t.Errorf("other errors stay 500: %d", rec.Code)
	}
}

func TestPickRouteTakesTheRepo(t *testing.T) {
	f, h := setup(t)
	rec := do(t, h, "GET", "/v1/pick?repo=KARMAX", "full", "")
	var out daemon.PickReply
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Agent != "agent-02" || len(out.Candidates) != 1 {
		t.Fatalf("body = %s", rec.Body)
	}
	if f.calls[len(f.calls)-1] != "pick KARMAX" {
		t.Errorf("calls = %v", f.calls)
	}
}
