package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/memory"
)

// The fake fleet's agents (fake.Status): agent-02 works on T1, the
// operator's; agent-03 on T-ram, a tailnet member's; agent-05 has no task.

func (f *fake) Recall(ctx context.Context, q string, limit int) (*memory.Result, error) {
	f.calls = append(f.calls, fmt.Sprintf("recall %q %d", q, limit))
	if f.mem != nil {
		return f.mem.Recall(ctx, q, limit)
	}
	return &memory.Result{Query: q, Memories: []memory.Memory{{Path: "people/ram.md", Content: "Ram runs the build host."}}}, nil
}

func (f *fake) Suggest(_ context.Context, from, text, why string, _ time.Time) (string, error) {
	f.calls = append(f.calls, "suggest "+from+" "+text+" / "+why)
	return "karmax", nil
}

func (f *fake) OperatorAssigner(a string) bool { return a != "" && !strings.Contains(a, "@") }

func TestTheOperatorAndTheOrchestratorRecallAndSuggest(t *testing.T) {
	f, h := setup(t)
	rec := do(t, h, "GET", "/v1/memory/recall?q=who+is+ram&limit=3", "full", "")
	var res memory.Result
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &res) != nil || len(res.Memories) != 1 || res.Memories[0].Path != "people/ram.md" {
		t.Fatalf("recall: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "POST", "/v1/memory/suggest", "full", `{"from":"karmax","text":"Ram prefers squash merges","why":"review"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"to":"karmax"`) {
		t.Fatalf("suggest: %d %s", rec.Code, rec.Body)
	}
	do(t, h, "POST", "/v1/memory/suggest", "full", `{"text":"x"}`)
	want := `recall "who is ram" 3|suggest karmax Ram prefers squash merges / review|suggest operator x / `
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("calls = %s", got)
	}
}

// An agent may read the operator's memory only while it works on a task the
// operator's side gave it, and fleetd decides that from the ledger.
func TestAnAgentUsesMemoryOnlyOnTheOperatorsTask(t *testing.T) {
	f, h := setup(t)
	cases := []struct {
		agent string // "" sends no key
		want  int
	}{
		{"agent-02", 200}, // T1, the operator's
		{"agent-03", 403}, // T-ram, a member's
		{"agent-05", 403}, // no task
		{"", 403},         // who is asking is unproven
	}
	for _, c := range cases {
		for _, r := range []struct{ method, path, body string }{
			{"GET", "/v1/memory/recall?q=ram", ""},
			{"POST", "/v1/memory/suggest", `{"text":"a fact"}`},
		} {
			req := newReq(r.method, r.path, "relay", r.body)
			if c.agent != "" {
				asAgent(req, c.agent)
			}
			if resp := serve(h, req); resp.StatusCode != c.want {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("%s %s as %q: %d %s, want %d", r.method, r.path, c.agent, resp.StatusCode, b, c.want)
			}
		}
	}
	// The agent suggests as itself, whatever it says.
	ado(t, h, "POST", "/v1/memory/suggest", "agent-02", `{"from":"operator","text":"a fact","why":"seen"}`)
	if got := f.calls[len(f.calls)-1]; got != "suggest agent-02 a fact / seen" {
		t.Fatalf("call = %q", got)
	}
	if n := strings.Count(strings.Join(f.calls, "|"), "recall"); n != 1 {
		t.Fatalf("recall reached the fleet %d times: %q", n, f.calls)
	}
}

// A colleague on the tailnet never reads the operator's memory, not even
// about their own task; the console has no business with it either.
func TestAMemberAndTheConsoleNeverUseMemory(t *testing.T) {
	f, h, _ := tailnetSetup(t)
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/v1/memory/recall?q=ram", ""},
		{"POST", "/v1/memory/suggest", `{"text":"a fact"}`},
	} {
		if rec := tdo(t, h, r.method, r.path, ramIP, "", r.body); rec.Code != 403 {
			t.Errorf("member %s %s: %d", r.method, r.path, rec.Code)
		}
		if rec := tdo(t, h, r.method, r.path, opIP, "", r.body); rec.Code != 200 {
			t.Errorf("tailnet operator %s %s: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
		if rec := do(t, h, r.method, r.path, "console", r.body); rec.Code != 403 {
			t.Errorf("console %s %s: %d", r.method, r.path, rec.Code)
		}
	}
	if got := strings.Join(f.calls, "|"); got != `recall "ram" 0|suggest operator a fact / ` {
		t.Fatalf("calls = %s", got)
	}
}

func TestRecallNeedsAQueryAndMemoryOffSaysSo(t *testing.T) {
	_, h := setup(t)
	if rec := do(t, h, "GET", "/v1/memory/recall", "full", ""); rec.Code != 400 {
		t.Errorf("no query: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/v1/memory/recall?q=x&limit=-1", "full", ""); rec.Code != 400 {
		t.Errorf("bad limit: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/v1/memory/suggest", "full", `{"text":" "}`); rec.Code != 400 {
		t.Errorf("empty suggestion: %d", rec.Code)
	}
}

// Through the whole API: gitloom gets the key on its read route, and no
// answer or failure brings it back, even one that quotes it.
func TestTheGitloomKeyNeverLeavesFleetd(t *testing.T) {
	const key = "gl_secret_key_42"
	var seen []string
	reply := `{"memories":[{"path":"a.md","content":"echo $AUTH"}]}`
	status := 200
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, strings.ReplaceAll(reply, "$AUTH", r.Header.Get("Authorization")))
	}))
	defer gl.Close()
	f, h := setup(t)
	f.mem = &memory.Client{URL: gl.URL, Key: key, Namespace: "mira"}
	rec := do(t, h, "GET", "/v1/memory/recall?q=x", "full", "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), key) || !strings.Contains(rec.Body.String(), "echo") {
		t.Fatalf("recall: %d %s", rec.Code, rec.Body)
	}
	status, reply = 401, `{"error":{"code":"bad_key","message":"key $AUTH is revoked"}}`
	rec = do(t, h, "GET", "/v1/memory/recall?q=x", "full", "")
	if rec.Code < 400 || strings.Contains(rec.Body.String(), key) || !strings.Contains(rec.Body.String(), "revoked") {
		t.Fatalf("failed recall: %d %s", rec.Code, rec.Body)
	}
	if len(seen) != 2 || seen[0] != "GET /v1/retrieve Bearer "+key {
		t.Fatalf("gitloom saw %q", seen)
	}
}
