package outbound

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// KARMAX's webhook server verifies hex(hmac-sha256(secret, body)), with or
// without a "sha256=" prefix, in the route's signature header.
func TestWebhookIsSignedTheWayKarmaxVerifies(t *testing.T) {
	var gotSig, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotSig = string(b), r.Header.Get("X-Fleet-Signature")
	}))
	defer srv.Close()
	p := Webhook{URL: srv.URL, Secret: "s3cret", Client: srv.Client()}
	if err := p.Push(context.Background(), "fleet.agent.cooling", "agent-03", "rate-limited", reconcile.State{Name: "agent-03", Task: "T1"}); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(gotBody))
	if gotSig != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature %q does not verify", gotSig)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(gotBody), &body)
	if body["event"] != "fleet.agent.cooling" || body["agent"] != "agent-03" || body["task"] != "T1" || body["text"] != "rate-limited" {
		t.Fatalf("body = %s", gotBody)
	}
}

func TestWebhookFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	if err := (Webhook{URL: srv.URL, Client: srv.Client()}).Push(context.Background(), "e", "a", "t", reconcile.State{}); err == nil {
		t.Fatal("a 401 must be an error")
	}
}

func TestNotifyArgv(t *testing.T) {
	var got []string
	n := Karmax{Bin: "karmax", run: func(_ context.Context, argv []string) error { got = argv; return nil }}
	if err := n.Notify(context.Background(), "agent-03 disabled"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"karmax", "notify", "--kind", "alert", "Agent fleet", "agent-03 disabled"}) {
		t.Fatalf("argv = %q", got)
	}
}

// harness.list reports the orchestrator's own windows as "42%" strings.
func TestOrchestratorQuotaFromHarnessList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tools/harness.list" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(403)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"output":{"enabled":true,"rate_limits":{
			"five_hour":{"used":"42%","resets_at":"2026-10-08T15:00:00Z"},
			"seven_day":{"used":"9%","resets_at":"2026-10-12T00:00:00Z"}}}}`))
	}))
	defer srv.Close()
	q, err := (Karmax{APIURL: srv.URL, APIToken: "tok", Client: srv.Client()}).Quota(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q.FiveHour != 42 || q.SevenDay != 9 || q.FiveHourResets.Format(time.RFC3339) != "2026-10-08T15:00:00Z" {
		t.Fatalf("quota = %+v", q)
	}
}

func TestDashboardWritesOnlyIntoAnExistingDashboard(t *testing.T) {
	root := t.TempDir()
	d := Dashboard{DataDir: root, ID: "fleet"}
	if err := d.Write(map[string]any{"agents": []int{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "dashboards", "fleet")); !os.IsNotExist(err) {
		t.Fatal("created a dashboard; it is made once with KARMAX's dashboard tool")
	}
	_ = os.MkdirAll(filepath.Join(root, "dashboards", "fleet"), 0o755)
	if err := d.Write(map[string]any{"agents": []int{1, 2}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "dashboards", "fleet", "data", "agents.json"))
	if err != nil || strings.TrimSpace(string(b)) != "[1,2]" {
		t.Fatalf("agents.json = %q, %v", b, err)
	}
}

func TestEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	_ = os.WriteFile(p, []byte("# tokens\nexport A=1\nB=\"two words\"\nC='x=y'\n\nbad line\nD=4 # note\n"), 0o600)
	env, err := ReadEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	if env["A"] != "1" || env["B"] != "two words" || env["C"] != "x=y" || env["D"] != "4" || len(env) != 4 {
		t.Fatalf("env = %v", env)
	}
}

func TestParseDockerStats(t *testing.T) {
	out := `{"Name":"agent-01","CPUPerc":"12.50%","MemUsage":"1.5GiB / 15.5GiB","MemPerc":"9.68%"}
{"Name":"agent-02","CPUPerc":"0.00%","MemUsage":"512MiB / 15.5GiB","MemPerc":"3.2%"}
`
	s := ParseDockerStats([]byte(out), time.Unix(100, 0))
	if len(s) != 2 || s[0].Container != "agent-01" || s[0].CPU != 12.5 || s[0].MemBytes != 1610612736 || s[1].MemBytes != 536870912 {
		t.Fatalf("samples = %+v", s)
	}
}
