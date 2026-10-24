package main

import (
	"bufio"
	"encoding/json"
	"io"
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

func TestMCPListsAndCallsBothMemoryTools(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{
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
	init := got[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" || init["capabilities"].(map[string]any)["tools"] == nil ||
		init["serverInfo"].(map[string]any)["name"] != "fleet" {
		t.Fatalf("initialize = %v", init)
	}
	var names []string
	for _, tool := range got[1]["result"].(map[string]any)["tools"].([]any) {
		tm := tool.(map[string]any)
		names = append(names, tm["name"].(string))
		if tm["description"] == "" || tm["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("tool %v lacks a description or schema", tm)
		}
	}
	if strings.Join(names, ",") != "memory_recall,memory_suggest" {
		t.Fatalf("tools = %v", names)
	}
	recall := got[2]["result"].(map[string]any)
	if got[2]["id"].(float64) != 3 || recall["isError"] == true || !strings.Contains(text(recall), "Ram runs the build host.") {
		t.Fatalf("recall = %v", got[2])
	}
	if got[3]["id"] != "s" || !strings.Contains(text(got[3]["result"].(map[string]any)), "karmax") {
		t.Fatalf("suggest = %v", got[3])
	}
	if f.reqs[0] != "GET /v1/memory/recall?limit=3&q=who+is+ram " || !strings.HasPrefix(f.reqs[1], "POST /v1/memory/suggest ") ||
		reqBody(t, f.reqs[1])["text"] != "Ram prefers squash merges" {
		t.Fatalf("fleetd saw %q", f.reqs)
	}
}

func text(result map[string]any) string {
	var s string
	for _, c := range result["content"].([]any) {
		s += c.(map[string]any)["text"].(string)
	}
	return s
}

func TestMCPReportsFailuresAsResultsAndBadCallsAsErrors(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{}}
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
	if r := got[0]["result"].(map[string]any); r["isError"] != true || !strings.Contains(text(r), "query") {
		t.Errorf("recall without a query = %v", got[0])
	}
	for i, code := range map[int]float64{1: -32602, 2: -32601, 3: -32700} {
		if e, ok := got[i]["error"].(map[string]any); !ok || e["code"] != code {
			t.Errorf("reply %d = %v, want error %v", i, got[i], code)
		}
	}
	if _, ok := got[4]["result"]; !ok {
		t.Errorf("ping = %v", got[4])
	}
	if len(f.reqs) != 0 {
		t.Errorf("fleetd was asked %q", f.reqs)
	}
}
