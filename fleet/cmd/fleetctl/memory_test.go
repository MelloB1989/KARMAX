package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/fleet/internal/identity"
)

const recallReply = `{"namespace":"mira","query":"ram","memories":[` +
	`{"path":"people/ram.md","title":"Ram","content":"Ram runs the build host.\nHe prefers squash merges.","score":0.91}]}`

func TestMemoryRecallAsksFleetdAndPrintsWhatItFound(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/memory/recall": recallReply}}
	out, err := fleetctl(t, f, "memory", "recall", "who is ram", "--limit", "3")
	if err != nil {
		t.Fatal(err)
	}
	if f.reqs[0] != "GET /v1/memory/recall?limit=3&q=who+is+ram " {
		t.Fatalf("request = %q", f.reqs[0])
	}
	for _, want := range []string{"people/ram.md", "Ram", "Ram runs the build host.", "  He prefers squash merges."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	out, _ = fleetctl(t, f, "memory", "recall", "ram", "--json")
	if !strings.Contains(out, `"path": "people/ram.md"`) {
		t.Fatalf("--json = %s", out)
	}
	f.reply["/v1/memory/recall"] = `{"query":"zed","memories":[]}`
	out, _ = fleetctl(t, f, "memory", "recall", "zed")
	if !strings.Contains(out, "nothing") {
		t.Fatalf("no memories = %q", out)
	}
}

func TestMemorySuggestSendsTheFactAndWhy(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/memory/suggest": `{"ok":true,"to":"karmax"}`}}
	out, err := fleetctl(t, f, "memory", "suggest", "Ram prefers squash merges", "--why", "he said so in review")
	if err != nil {
		t.Fatal(err)
	}
	in := reqBody(t, f.reqs[0])
	if !strings.HasPrefix(f.reqs[0], "POST /v1/memory/suggest ") || in["text"] != "Ram prefers squash merges" || in["why"] != "he said so in review" {
		t.Fatalf("request = %q", f.reqs[0])
	}
	if !strings.Contains(out, "karmax") {
		t.Fatalf("out = %q", out)
	}
}

func TestMemoryNeedsASubcommandAndText(t *testing.T) {
	for _, args := range [][]string{{"memory"}, {"memory", "forget", "x"}, {"memory", "recall"}, {"memory", "suggest", " "}} {
		if _, err := fleetctl(t, &fakeFleetd{}, args...); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
}

// In an agent's container fleetctl proves which agent it is on every call.
func TestFleetctlSendsTheAgentsKey(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(identity.Header))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	t.Setenv("FLEETD_URL", srv.URL)
	t.Setenv("FLEET_TOKEN", "relay")
	t.Setenv("FLEET_CONFIG", t.TempDir()+"/none.yaml")
	t.Setenv("FLEET_AGENT", "agent-03")
	t.Setenv("FLEET_NAME", "agent-03:2")
	t.Setenv("FLEET_AGENT_KEY", "k3")
	c, err := newClient()
	if err != nil {
		t.Fatal(err)
	}
	_ = c.do("GET", "/v1/status", nil, nil)
	t.Setenv("FLEET_AGENT_KEY", "")
	c, _ = newClient()
	_ = c.do("GET", "/v1/status", nil, nil)
	if len(got) != 2 || got[0] != "agent-03:2:k3" || got[1] != "" {
		t.Fatalf("headers = %q", got)
	}
}
