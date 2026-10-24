package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// mcpSession runs `fleetctl mcp` against a fake fleetd: each line in is one
// JSON-RPC message, and it returns each line out, decoded.
func mcpSession(t *testing.T, f *fakeFleetd, lines ...string) []map[string]any {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := fleetctlIO(t, f, inR, outW, "mcp")
		outW.Close()
		done <- err
	}()
	go func() {
		for _, l := range lines {
			_, _ = io.WriteString(inW, l+"\n")
		}
		inW.Close()
	}()
	var got []map[string]any
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not one JSON message per line: %q", sc.Text())
		}
		got = append(got, m)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return got
}

// The whoami replies of each kind of caller.
const (
	whoOperator   = `{"scope":"full","name":"operator"}`
	whoTailnetOp  = `{"scope":"full","name":"op@example.com"}`
	whoAgent      = `{"scope":"relay","name":"agent"}`
	whoMember     = `{"scope":"member","name":"ram@example.com"}`
	whoNoOneKnown = `{"ok":true}`
)

// fleetdSaw is what fleetd was asked, sorted, but who the caller is.
func fleetdSaw(f *fakeFleetd) []string {
	var out []string
	for _, r := range f.reqs {
		if !strings.HasPrefix(r, "GET /v1/whoami ") {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out
}

func TestMCPListsAndCallsBothMemoryTools(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{
		"/v1/whoami":         whoOperator,
		"/v1/memory/recall":  recallReply,
		"/v1/memory/suggest": `{"ok":true,"to":"karmax"}`,
	}}
	got := mcpSession(t, f,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"2"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory_recall","arguments":{"query":"who is ram","limit":3}}}`,
		`{"jsonrpc":"2.0","id":"s","method":"tools/call","params":{"name":"memory_suggest","arguments":{"fact":"Ram prefers squash merges","why":"review"}}}`,
	)
	if len(got) != 4 {
		t.Fatalf("replies = %v (a notification gets none)", got)
	}
	// Tool calls run side by side, so replies come in any order: by id.
	r := byID(got)
	init := r["1"]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" || init["capabilities"].(map[string]any)["tools"] == nil ||
		init["serverInfo"].(map[string]any)["name"] != "fleet" {
		t.Fatalf("initialize = %v", init)
	}
	var names []string
	for _, tool := range r["2"]["result"].(map[string]any)["tools"].([]any) {
		tm := tool.(map[string]any)
		names = append(names, tm["name"].(string))
		if tm["description"] == "" || tm["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("tool %v lacks a description or schema", tm)
		}
	}
	if strings.Join(names, ",") != "memory_recall,memory_suggest,karmax_tools,karmax_call" {
		t.Fatalf("tools = %v", names)
	}
	recall := r["3"]["result"].(map[string]any)
	if recall["isError"] == true || !strings.Contains(text(recall), "Ram runs the build host.") {
		t.Fatalf("recall = %v", r["3"])
	}
	if !strings.Contains(text(r[`"s"`]["result"].(map[string]any)), "karmax") {
		t.Fatalf("suggest = %v", r[`"s"`])
	}
	saw := fleetdSaw(f)
	if saw[0] != "GET /v1/memory/recall?limit=3&q=who+is+ram " || !strings.HasPrefix(saw[1], "POST /v1/memory/suggest ") ||
		reqBody(t, saw[1])["text"] != "Ram prefers squash merges" {
		t.Fatalf("fleetd saw %q", f.reqs)
	}
}

// byID is the replies keyed by their id's JSON (1, "s", null).
func byID(got []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, m := range got {
		id, _ := json.Marshal(m["id"])
		out[string(id)] = m
	}
	return out
}

func text(result map[string]any) string {
	var s string
	for _, c := range result["content"].([]any) {
		s += c.(map[string]any)["text"].(string)
	}
	return s
}

func TestMCPReportsFailuresAsResultsAndBadCallsAsErrors(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/whoami": whoOperator}}
	got := mcpSession(t, f,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_recall","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory_forget","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`,
		`not json`,
		`{"jsonrpc":"2.0","id":4,"method":"ping"}`,
	)
	if len(got) != 5 {
		t.Fatalf("replies = %v", got)
	}
	r := byID(got)
	if res := r["1"]["result"].(map[string]any); res["isError"] != true || !strings.Contains(text(res), "query") {
		t.Errorf("recall without a query = %v", r["1"])
	}
	for id, code := range map[string]float64{"2": -32602, "3": -32601, "null": -32700} {
		if e, ok := r[id]["error"].(map[string]any); !ok || e["code"] != code {
			t.Errorf("reply %s = %v, want error %v", id, r[id], code)
		}
	}
	if _, ok := r["4"]["result"]; !ok {
		t.Errorf("ping = %v", r["4"])
	}
	if saw := fleetdSaw(f); len(saw) != 0 {
		t.Errorf("fleetd was asked %q", saw)
	}
}

func TestMCPCallsKARMAXToolsThroughFleetdAndWaitsOutAWrite(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{
		"/v1/whoami":       whoAgent,
		"/v1/karmax/tools": `[{"name":"google.mail.search","description":"search mail","class":"read"},{"name":"google.mail.send","class":"write"}]`,
		"/v1/karmax/call":  `{"tool":"google.mail.send","class":"write","approval":{"id":7,"state":"pending"}}`,
		"/v1/approvals/7":  `{"tool":"google.mail.send","class":"write","result":{"tool":"google.mail.send","ok":true,"output":{"id":"m1"}}}`,
	}}
	got := byID(mcpSession(t, f,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"karmax_tools","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"karmax_call","arguments":{"tool":"google.mail.send","args":{"to":"ram@example.com"},"reason":"reply to Ram","timeout":"10m"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"karmax_call","arguments":{"reason":"x"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"karmax_call","arguments":{"tool":"x.y","reason":"x","timeout":"soon"}}}`,
	))
	if tools := got["1"]["result"].(map[string]any); tools["isError"] == true || !strings.Contains(text(tools), `"class": "write"`) {
		t.Fatalf("karmax_tools = %v", got["1"])
	}
	if call := got["2"]["result"].(map[string]any); call["isError"] == true || text(call) != `{"id":"m1"}` {
		t.Fatalf("karmax_call = %v", got["2"])
	}
	for _, id := range []string{"3", "4"} {
		if r := got[id]["result"].(map[string]any); r["isError"] != true {
			t.Errorf("bad karmax_call %s = %v", id, got[id])
		}
	}
	saw := fleetdSaw(f)
	want := []string{"GET /v1/approvals/7?wait=5m&from=operator ", "GET /v1/karmax/tools "}
	if len(saw) != 3 || saw[0] != want[0] || saw[1] != want[1] || !strings.HasPrefix(saw[2], "POST /v1/karmax/call ") {
		t.Fatalf("fleetd saw %q", f.reqs)
	}
	body := reqBody(t, saw[2])
	if body["tool"] != "google.mail.send" || body["reason"] != "reply to Ram" || body["timeout"] != "10m0s" ||
		body["args"].(map[string]any)["to"] != "ram@example.com" {
		t.Fatalf("call body = %v", body)
	}
}

func TestMCPAToolErrorIsTheModelsToRead(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{
		"/v1/whoami":      whoOperator,
		"/v1/karmax/call": `{"tool":"slack.channels","class":"read","result":{"tool":"slack.channels","ok":false,"error":"slack is not connected"}}`,
	}}
	got := byID(mcpSession(t, f, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"karmax_call","arguments":{"tool":"slack.channels","reason":"find the channel"}}}`))
	if r := got["1"]["result"].(map[string]any); r["isError"] != true || !strings.Contains(text(r), "slack is not connected") {
		t.Fatalf("karmax_call = %v", got["1"])
	}
}

// A write waiting for its approval holds its own call, not the session.
func TestMCPAnswersWhileAWriteWaitsForApproval(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/whoami" {
			_, _ = io.WriteString(w, whoOperator)
			return
		}
		if r.URL.Path == "/v1/karmax/call" {
			_, _ = io.WriteString(w, `{"tool":"comms.send","class":"write","approval":{"id":9,"state":"pending"}}`)
			return
		}
		<-release
		_, _ = io.WriteString(w, `{"tool":"comms.send","class":"write","result":{"tool":"comms.send","ok":true,"output":"sent"}}`)
	}))
	defer srv.Close()
	t.Setenv("FLEETD_URL", srv.URL)
	t.Setenv("FLEET_TOKEN", "tok")
	t.Setenv("FLEET_AGENT_KEY", "")
	t.Setenv("FLEET_CONFIG", t.TempDir()+"/none.yaml")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveMCP(inR, outW); outW.Close() }()
	lines := bufio.NewScanner(outR)
	next := func() map[string]any {
		t.Helper()
		if !lines.Scan() {
			t.Fatal("the server said nothing more")
		}
		var m map[string]any
		_ = json.Unmarshal(lines.Bytes(), &m)
		return m
	}
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"karmax_call","arguments":{"tool":"comms.send","reason":"tell Ram"}}}`+"\n")
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n")
	if m := next(); m["id"] != float64(2) {
		t.Fatalf("first reply = %v, want the ping's while the write waits", m)
	}
	close(release)
	if m := next(); m["id"] != float64(1) || text(m["result"].(map[string]any)) != `"sent"` {
		t.Fatalf("the write's reply = %v", m)
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A session lists only the tools fleetd would let its caller use: the
// operator (on a token or the tailnet) and an agent get them all, a member
// none of the operator's, nor does anyone fleetd cannot place.
func TestMCPListsEachScopeOnlyTheToolsFleetdGivesIt(t *testing.T) {
	all := "memory_recall,memory_suggest,karmax_tools,karmax_call"
	for who, want := range map[string]string{
		whoOperator: all, whoTailnetOp: all, whoAgent: all, whoMember: "", whoNoOneKnown: "",
	} {
		f := &fakeFleetd{reply: map[string]string{"/v1/whoami": who}}
		r := byID(mcpSession(t, f,
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		))
		tools, ok := r["2"]["result"].(map[string]any)["tools"].([]any)
		if !ok {
			t.Fatalf("%s: tools/list = %v (want a list, even empty)", who, r["2"])
		}
		var names []string
		for _, tool := range tools {
			names = append(names, tool.(map[string]any)["name"].(string))
		}
		if strings.Join(names, ",") != want {
			t.Errorf("%s: tools = %v, want %s", who, names, want)
		}
		instr := r["1"]["result"].(map[string]any)["instructions"].(string)
		if (want == "") != !strings.Contains(instr, "memory_recall reads memory") {
			t.Errorf("%s: instructions = %q", who, instr)
		}
	}
}

// A member who calls a tool they were not shown is refused here, and fleetd
// is never asked for it.
func TestMCPRefusesAMemberAHiddenTool(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/whoami": whoMember}}
	r := byID(mcpSession(t, f,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_recall","arguments":{"query":"who is ram"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"karmax_call","arguments":{"tool":"google.mail.send","reason":"x"}}}`,
	))
	for _, id := range []string{"1", "2"} {
		e, ok := r[id]["error"].(map[string]any)
		if !ok || e["code"] != float64(-32602) || !strings.Contains(e["message"].(string), "not for scope member") {
			t.Errorf("call %s = %v", id, r[id])
		}
	}
	if saw := fleetdSaw(f); len(saw) != 0 {
		t.Errorf("fleetd was asked %q", saw)
	}
	if len(f.reqs) != 1 {
		t.Errorf("whoami asked %d times, want once a session", len(f.reqs))
	}
}
