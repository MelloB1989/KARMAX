package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/outbound"
)

// The fake Ops's gateway: the scope tests reach it; the gateway's own tests
// below run the real fleet behind the API.
func (f *fake) KarmaxTools(context.Context) ([]daemon.GatewayTool, error) {
	return []daemon.GatewayTool{}, nil
}
func (f *fake) KarmaxCall(_ context.Context, who daemon.Requester, tool string, _ map[string]any, _ string, _ time.Duration, _ time.Time) (daemon.CallReply, error) {
	f.calls = append(f.calls, "karmax "+who.Name+" "+who.Task+" "+tool)
	return daemon.CallReply{Tool: tool, Class: "read"}, nil
}
func (f *fake) AwaitApproval(context.Context, daemon.Requester, int64) (daemon.CallReply, error) {
	return daemon.CallReply{}, nil
}
func (f *fake) Approvals(string, int) ([]ledger.Approval, error) { return []ledger.Approval{}, nil }
func (f *fake) Decide(id int64, by string, approve bool, reason string, _ time.Time) (ledger.Approval, error) {
	return ledger.Approval{ID: id, DecidedBy: by}, nil
}

const karmaxToken = "karmax-api-token-only-fleetd-holds"

// karmaxAPI is a fake KARMAX API: its tools, and every call it was made.
type karmaxAPI struct {
	mu    sync.Mutex
	calls []string // tool name, then the arguments' JSON
	auth  []string // each request's Authorization
}

func (k *karmaxAPI) called() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.calls...)
}

func (k *karmaxAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	k.auth = append(k.auth, r.Header.Get("Authorization"))
	k.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+karmaxToken {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet && r.URL.Path == "/api/tools" {
		var tools []map[string]string
		for _, n := range []string{"google.mail.search", "google.mail.send", "shell.exec", "memory.ingest", "slack.channels", "whatsapp.read"} {
			tools = append(tools, map[string]string{"name": n, "description": "the " + n + " tool"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"agent": "mira", "tools": tools})
		return
	}
	name, ok := strings.CutPrefix(r.URL.Path, "/api/tools/")
	if r.Method != http.MethodPost || !ok || r.URL.RawQuery != "" {
		http.Error(w, `{"error":"not here"}`, http.StatusNotFound)
		return
	}
	b, _ := io.ReadAll(r.Body)
	k.mu.Lock()
	k.calls = append(k.calls, name+" "+string(b))
	k.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"tool": name, "ok": true, "output": map[string]any{"messages": 2}})
}

// orchBox is a container that only relays: the orchestrator's, and the
// agents' (their briefs).
type orchBox struct {
	daemon.Box
	mu   sync.Mutex
	sent []string
}

func (b *orchBox) Relay(_ context.Context, to, from, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, from+"→"+to+": "+text)
	return nil
}

func (b *orchBox) messages() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sent...)
}

const gatewayYAML = `home: /home/op
hosts: {kali: {exec: [docker]}}
orchestrator: {host: kali, token_env: O}
agents:
  agent-03: {host: kali, token_env: T3}
  agent-05: {host: kali, token_env: T5}
  agent-07: {host: kali, token_env: T7}
tailnet: {listen: "100.64.0.1:7878", operators: [op@example.com], members: ["*@example.com"]}
karmax:
  tools: {slack.channels: read}
`

type gatewayFleet struct {
	fleet  *daemon.Fleet
	karmax *karmaxAPI
	orch   *orchBox
	h      http.Handler // tokens full and relay
	tail   http.Handler // the tailnet listener
}

// gatewaySetup is the real fleet behind the API, KARMAX faked: agent-03 on
// the operator's task T-op, agent-05 on a member's task T-ram, agent-07 on
// none.
func gatewaySetup(t *testing.T) *gatewayFleet {
	t.Helper()
	cfg, err := config.Parse([]byte(gatewayYAML))
	if err != nil {
		t.Fatal(err)
	}
	db, err := ledger.Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	g := &gatewayFleet{karmax: &karmaxAPI{}, orch: &orchBox{}}
	agents := &orchBox{}
	g.fleet, err = daemon.New(cfg, db, func(name string) daemon.Box {
		if name == "karmax" {
			return g.orch
		}
		return agents
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.karmax)
	t.Cleanup(srv.Close)
	g.fleet.Karmax = outbound.Karmax{APIURL: srv.URL, APIToken: karmaxToken}
	ctx := context.Background()
	if err := g.fleet.Assign(ctx, "agent-03", "T-op", "operator", "Task T-op from operator. Tidy the operator's inbox.\nMore detail.", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := g.fleet.Assign(ctx, "agent-05", "T-ram", "ram@example.com", "Task T-ram from ram@example.com. Read the mail.", time.Now()); err != nil {
		t.Fatal(err)
	}
	g.h = Handler(g.fleet, Tokens{Full: "full", Relay: "relay"}, nil)
	who := fakeWho{ramIP: {Login: "ram@example.com"}, opIP: {Login: "op@example.com"}}
	g.tail = Tailnet(g.h, who, cfg.Tailnet)
	return g
}

// agentDo is a request from an agent: the relay token, and its proved name.
func agentDo(t *testing.T, h http.Handler, method, path, agent, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer relay")
	if agent != "" {
		asAgent(req, agent)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("%d %s: %v", rec.Code, rec.Body, err)
	}
	return v
}

func TestAReadPassesThroughWithKARMAXsTokenWhichNeverReachesTheAgent(t *testing.T) {
	g := gatewaySetup(t)
	rec := agentDo(t, g.h, "POST", "/v1/karmax/call", "agent-03",
		`{"tool":"google.mail.search","args":{"query":"invoice"},"reason":"find the invoice"}`)
	if rec.Code != 200 {
		t.Fatalf("read: %d %s", rec.Code, rec.Body)
	}
	got := decode[daemon.CallReply](t, rec)
	if got.Class != "read" || got.Result == nil || !got.Result.OK || got.Approval != nil {
		t.Fatalf("read reply = %s", rec.Body)
	}
	if calls := g.karmax.called(); len(calls) != 1 || calls[0] != `google.mail.search {"query":"invoice"}` {
		t.Fatalf("KARMAX was called %q", calls)
	}
	g.karmax.mu.Lock()
	auth := g.karmax.auth[0]
	g.karmax.mu.Unlock()
	if auth != "Bearer "+karmaxToken {
		t.Fatalf("fleetd called KARMAX with %q", auth)
	}
	// Nothing an agent can call shows it KARMAX's token.
	for _, r := range []*httptest.ResponseRecorder{rec,
		agentDo(t, g.h, "GET", "/v1/karmax/tools", "agent-03", ""),
		agentDo(t, g.h, "POST", "/v1/karmax/call", "agent-03", `{"tool":"no.such_tool_list"}`),
	} {
		dump, _ := json.Marshal(r.Header())
		if strings.Contains(r.Body.String()+string(dump), karmaxToken) {
			t.Fatalf("KARMAX's token reached the agent: %s", r.Body)
		}
	}
	evs, _ := g.fleet.Events(daemon.EventsQuery{Agent: "agent-03", Kinds: []string{"karmax.call"}})
	if len(evs) == 0 || !strings.Contains(evs[0].Detail, `"decision":"ran"`) || !strings.Contains(evs[0].Detail, `"task":"T-op"`) ||
		!strings.Contains(evs[0].Detail, `"args":["query"]`) || strings.Contains(evs[0].Detail, "invoice") {
		t.Fatalf("audit = %+v, want the call with its argument names and not their values", evs)
	}
}

func TestToolsListsWhatAnAgentMayCallWithItsClass(t *testing.T) {
	g := gatewaySetup(t)
	rec := agentDo(t, g.h, "GET", "/v1/karmax/tools", "agent-03", "")
	if rec.Code != 200 {
		t.Fatalf("tools: %d %s", rec.Code, rec.Body)
	}
	got := map[string]string{}
	for _, tl := range decode[[]daemon.GatewayTool](t, rec) {
		got[tl.Name] = tl.Class
	}
	want := map[string]string{"google.mail.search": "read", "google.mail.send": "write", "slack.channels": "read", "whatsapp.read": "read"}
	if len(got) != len(want) {
		t.Fatalf("tools = %v, want %v (shell.exec and memory.ingest left out)", got, want)
	}
	for n, c := range want {
		if got[n] != c {
			t.Errorf("%s = %q, want %q", n, got[n], c)
		}
	}
}

// file asks for a write as agent-03 and returns the pending request.
func file(t *testing.T, g *gatewayFleet, timeout string) ledger.Approval {
	t.Helper()
	rec := agentDo(t, g.h, "POST", "/v1/karmax/call", "agent-03",
		`{"tool":"google.mail.send","args":{"to":"ram@example.com","body":"hello"},"reason":"reply to Ram","timeout":"`+timeout+`"}`)
	if rec.Code != 200 {
		t.Fatalf("write: %d %s", rec.Code, rec.Body)
	}
	got := decode[daemon.CallReply](t, rec)
	if got.Class != "write" || got.Approval == nil || got.Approval.State != ledger.Pending || got.Result != nil {
		t.Fatalf("write reply = %s", rec.Body)
	}
	return *got.Approval
}

func TestAWriteWaitsForTheOrchestratorsApprovalThenRuns(t *testing.T) {
	g := gatewaySetup(t)
	a := file(t, g, "")
	if len(g.karmax.called()) != 0 {
		t.Fatal("a write ran before it was approved")
	}
	msgs := g.orch.messages()
	if len(msgs) != 1 {
		t.Fatalf("the orchestrator was told %q", msgs)
	}
	for _, want := range []string{"agent-03→karmax", "google.mail.send", `"to":"ram@example.com"`, "reply to Ram",
		"Tidy the operator's inbox.", "fleetctl approve " + itoa(a.ID), "fleetctl deny " + itoa(a.ID), "WhatsApp"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("the orchestrator's message lacks %q:\n%s", want, msgs[0])
		}
	}
	if strings.Contains(msgs[0], "More detail.") {
		t.Errorf("the message carries more than the brief's first line:\n%s", msgs[0])
	}
	if rec := do(t, g.h, "GET", "/v1/approvals?state=pending", "full", ""); !strings.Contains(rec.Body.String(), `"tool":"google.mail.send"`) {
		t.Fatalf("approvals = %d %s", rec.Code, rec.Body)
	}

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- agentDo(t, g.h, "GET", "/v1/approvals/"+itoa(a.ID)+"?wait=1m", "agent-03", "") }()
	select {
	case rec := <-done:
		t.Fatalf("the wait ended before a decision: %d %s", rec.Code, rec.Body)
	case <-time.After(700 * time.Millisecond):
	}
	if rec := do(t, g.h, "POST", "/v1/approvals/"+itoa(a.ID)+"/approve", "full", `{"by":"karmax"}`); rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	rec := <-done
	got := decode[daemon.CallReply](t, rec)
	if rec.Code != 200 || got.Result == nil || !got.Result.OK {
		t.Fatalf("after approval: %d %s", rec.Code, rec.Body)
	}
	if calls := g.karmax.called(); len(calls) != 1 || !strings.HasPrefix(calls[0], "google.mail.send ") {
		t.Fatalf("KARMAX was called %q", calls)
	}
	// It ran once: waiting again does not run it again.
	if rec := agentDo(t, g.h, "GET", "/v1/approvals/"+itoa(a.ID)+"?wait=1s", "agent-03", ""); rec.Code != 403 || len(g.karmax.called()) != 1 {
		t.Fatalf("a second wait: %d %s, %d calls", rec.Code, rec.Body, len(g.karmax.called()))
	}
	evs, _ := g.fleet.Events(daemon.EventsQuery{Agent: "agent-03", Kinds: []string{"karmax.call", "karmax.decision"}})
	var kinds []string
	for _, e := range evs {
		if strings.Contains(e.Detail, "hello") || strings.Contains(e.Detail, "ram@example.com") {
			t.Errorf("an event carries an argument's value: %s", e.Detail)
		}
		var d map[string]any
		_ = json.Unmarshal([]byte(e.Detail), &d)
		kinds = append(kinds, e.Kind+" "+d["decision"].(string))
	}
	if strings.Join(kinds, ", ") != "karmax.call requested, karmax.decision approved, karmax.call approved" {
		t.Fatalf("audit = %v", kinds)
	}
}

func TestADeniedWriteIsRefusedWithTheReason(t *testing.T) {
	g := gatewaySetup(t)
	a := file(t, g, "")
	if rec := do(t, g.h, "POST", "/v1/approvals/"+itoa(a.ID)+"/deny", "full", `{"by":"karmax"}`); rec.Code != 400 {
		t.Fatalf("a deny without a reason: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, g.h, "POST", "/v1/approvals/"+itoa(a.ID)+"/deny", "full", `{"by":"karmax","reason":"the operator said no"}`); rec.Code != 200 {
		t.Fatalf("deny: %d %s", rec.Code, rec.Body)
	}
	rec := agentDo(t, g.h, "GET", "/v1/approvals/"+itoa(a.ID)+"?wait=1m", "agent-03", "")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "the operator said no") {
		t.Fatalf("after a deny: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, g.h, "POST", "/v1/approvals/"+itoa(a.ID)+"/approve", "full", `{"by":"karmax"}`); rec.Code != 409 {
		t.Fatalf("approving a denied request: %d %s", rec.Code, rec.Body)
	}
	if n := len(g.karmax.called()); n != 0 {
		t.Fatalf("a denied write ran (%d calls)", n)
	}
}

func TestAWriteNobodyDecidesExpires(t *testing.T) {
	g := gatewaySetup(t)
	a := file(t, g, "1s")
	rec := agentDo(t, g.h, "GET", "/v1/approvals/"+itoa(a.ID)+"?wait=1m", "agent-03", "")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("after the timeout: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, g.h, "POST", "/v1/approvals/"+itoa(a.ID)+"/approve", "full", `{"by":"karmax"}`); rec.Code != 409 {
		t.Fatalf("approving an expired request: %d %s", rec.Code, rec.Body)
	}
	if n := len(g.karmax.called()); n != 0 {
		t.Fatalf("an expired write ran (%d calls)", n)
	}
}

func TestADeniedClassToolIsRefusedWithoutAnApproval(t *testing.T) {
	g := gatewaySetup(t)
	for _, tool := range []string{"shell.exec", "memory.ingest", "file.write", "harness.send", "fleet.tell", "vault.secret_get"} {
		rec := agentDo(t, g.h, "POST", "/v1/karmax/call", "agent-03", `{"tool":"`+tool+`","args":{},"reason":"try"}`)
		if rec.Code != 403 {
			t.Errorf("%s: %d %s, want 403", tool, rec.Code, rec.Body)
		}
	}
	// The operator's side is held to it too.
	if rec := do(t, g.h, "POST", "/v1/karmax/call", "full", `{"tool":"shell.exec","args":{}}`); rec.Code != 403 {
		t.Errorf("shell.exec as full: %d", rec.Code)
	}
	if aps, _ := g.fleet.Approvals("", 0); len(aps) != 0 {
		t.Fatalf("a denied tool filed approvals: %+v", aps)
	}
	if len(g.karmax.called()) != 0 || len(g.orch.messages()) != 0 {
		t.Fatalf("a denied tool reached KARMAX or the orchestrator")
	}
}

func TestAnAgentCannotApprove(t *testing.T) {
	g := gatewaySetup(t)
	a := file(t, g, "")
	for _, path := range []string{"/approve", "/deny"} {
		if rec := agentDo(t, g.h, "POST", "/v1/approvals/"+itoa(a.ID)+path, "agent-03", `{"by":"karmax","reason":"x"}`); rec.Code != 403 {
			t.Errorf("an agent's %s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := agentDo(t, g.h, "GET", "/v1/approvals", "agent-03", ""); rec.Code != 403 {
		t.Errorf("an agent listed approvals: %d", rec.Code)
	}
	// Nor does a full-scope caller decide on their own request.
	rec := do(t, g.h, "POST", "/v1/karmax/call", "full", `{"tool":"google.mail.send","args":{},"reason":"mine","from":"operator"}`)
	own := decode[daemon.CallReply](t, rec).Approval
	if rec := do(t, g.h, "POST", "/v1/approvals/"+itoa(own.ID)+"/approve", "full", `{"by":"operator"}`); rec.Code != 403 {
		t.Errorf("approving one's own request: %d %s", rec.Code, rec.Body)
	}
	// Another agent cannot collect agent-03's request either.
	if _, err := g.fleet.Decide(a.ID, "karmax", true, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := agentDo(t, g.h, "GET", "/v1/approvals/"+itoa(a.ID)+"?wait=1s", "agent-07", ""); rec.Code != 403 {
		t.Errorf("another agent's wait: %d %s", rec.Code, rec.Body)
	}
	if len(g.karmax.called()) != 0 {
		t.Fatal("a request ran for someone who did not ask")
	}
}

func TestTheOperatorsSideMayUseTheGateway(t *testing.T) {
	g := gatewaySetup(t)
	if rec := do(t, g.h, "POST", "/v1/karmax/call", "full", `{"tool":"whatsapp.read","args":{}}`); rec.Code != 200 {
		t.Errorf("full token: %d %s", rec.Code, rec.Body)
	}
	if rec := tdo(t, g.tail, "GET", "/v1/karmax/tools", opIP, "", ""); rec.Code != 200 {
		t.Errorf("an operator on the tailnet: %d %s", rec.Code, rec.Body)
	}
}

func TestAMemberMayNeverUseTheGatewayNotEvenToRead(t *testing.T) {
	g := gatewaySetup(t)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/karmax/tools", ""},
		{"POST", "/v1/karmax/call", `{"tool":"whatsapp.read","args":{}}`},
		{"GET", "/v1/approvals", ""},
		{"POST", "/v1/approvals/1/approve", `{}`},
	} {
		if rec := tdo(t, g.tail, c.method, c.path, ramIP, "", c.body); rec.Code != 403 {
			t.Errorf("a member's %s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if len(g.karmax.called()) != 0 {
		t.Fatal("a member reached KARMAX")
	}
}

func TestAnAgentMayUseTheGatewayOnlyOnTheOperatorsWork(t *testing.T) {
	g := gatewaySetup(t)
	read := `{"tool":"whatsapp.read","args":{}}`
	if rec := agentDo(t, g.h, "POST", "/v1/karmax/call", "agent-03", read); rec.Code != 200 {
		t.Errorf("on the operator's task: %d %s", rec.Code, rec.Body)
	}
	if rec := agentDo(t, g.h, "POST", "/v1/karmax/call", "agent-05", read); rec.Code != 403 {
		t.Errorf("on a member's task: %d %s", rec.Code, rec.Body)
	}
	if rec := agentDo(t, g.h, "GET", "/v1/karmax/tools", "agent-05", ""); rec.Code != 403 {
		t.Errorf("tools on a member's task: %d %s", rec.Code, rec.Body)
	}
	if rec := agentDo(t, g.h, "POST", "/v1/karmax/call", "agent-07", read); rec.Code != 403 {
		t.Errorf("with no task: %d %s", rec.Code, rec.Body)
	}
	// A name the agent only claims is no name: the relay token alone does not
	// say which agent calls, and the body's from is not heard.
	if rec := agentDo(t, g.h, "POST", "/v1/karmax/call", "", `{"tool":"whatsapp.read","args":{},"from":"agent-03"}`); rec.Code != 403 {
		t.Errorf("unproved: %d %s", rec.Code, rec.Body)
	}
	// The console reads everything else; not this.
	if rec := do(t, Handler(g.fleet, Tokens{Full: "full", Relay: "relay", Console: "console"}, nil), "GET", "/v1/karmax/tools", "console", ""); rec.Code != 403 {
		t.Errorf("console: %d", rec.Code)
	}
	if n := len(g.karmax.called()); n != 1 {
		t.Fatalf("KARMAX was called %d times, want only agent-03's", n)
	}
	// A task that closes closes the door.
	if err := g.fleet.Close("agent-03", "T-op", "done", "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := agentDo(t, g.h, "POST", "/v1/karmax/call", "agent-03", read); rec.Code != 403 {
		t.Errorf("after done: %d %s", rec.Code, rec.Body)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
