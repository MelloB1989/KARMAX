package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/tailnet"
)

// fakeWho is tailscaled: an address's owner, by IP.
type fakeWho map[string]tailnet.Who

func (w fakeWho) WhoIs(_ context.Context, addr string) (tailnet.Who, error) {
	ip, _, _ := net.SplitHostPort(addr)
	if ip == "100.99.99.99" {
		return tailnet.Who{}, errors.New("dial unix tailscaled.sock: connection refused")
	}
	if who, ok := w[ip]; ok {
		return who, nil
	}
	return tailnet.Who{}, fmt.Errorf("%s: %w", ip, tailnet.ErrNotPeer)
}

const (
	ramIP     = "100.64.0.2" // a member
	opIP      = "100.64.0.3" // an operator
	outIP     = "100.64.0.4" // on the tailnet, not on the list
	taggedIP  = "100.64.0.5" // a subnet router
	strangeIP = "192.0.2.26" // not on the tailnet
)

func tailnetSetup(t *testing.T) (*fake, http.Handler, *config.Tailnet) {
	t.Helper()
	f, api := setup(t)
	cfg := &config.Tailnet{Listen: "100.64.0.1:7878", Operators: []string{"op@example.com"},
		Members: []string{"*@example.com"}, MemberOpen: 2, Dist: t.TempDir()}
	who := fakeWho{
		ramIP:    {Login: "ram@example.com", Name: "Ram", Node: "ram-laptop"},
		opIP:     {Login: "op@example.com", Node: "op-laptop"},
		outIP:    {Login: "stranger@other.org", Node: "stranger-laptop"},
		taggedIP: {Node: "subnet-router"},
	}
	return f, Tailnet(api, who, cfg), cfg
}

// tdo is a request from ip; token, when set, is sent too (and ignored).
func tdo(t *testing.T, h http.Handler, method, path, ip, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = net.JoinHostPort(ip, "40001")
	req.Host = "kali.tailnet-example.ts.net:7878"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTheTailnetKnowsWhoIsCalling(t *testing.T) {
	_, h, _ := tailnetSetup(t)
	for _, c := range []struct {
		ip, scope, name string
	}{
		{ramIP, "member", "ram@example.com"},
		{opIP, "full", "op@example.com"},
	} {
		rec := tdo(t, h, "GET", "/v1/whoami", c.ip, "", "")
		var got map[string]string
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got["scope"] != c.scope || got["name"] != c.name {
			t.Errorf("%s: %d %s", c.ip, rec.Code, rec.Body)
		}
	}
	for _, c := range []struct {
		ip   string
		code int
		says string
	}{
		{outIP, 403, "stranger@other.org may not use this fleet"},
		{taggedIP, 403, "tagged device"},
		{strangeIP, 403, "not on the tailnet"},
		{"100.99.99.99", 503, "cannot tell who you are"},
	} {
		rec := tdo(t, h, "GET", "/v1/status", c.ip, "full", "")
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.says) {
			t.Errorf("%s: %d %s", c.ip, rec.Code, rec.Body)
		}
	}
	// A token does not lift a member: the tailnet is who, not what you hold.
	if rec := tdo(t, h, "POST", "/v1/assign", ramIP, "full", `{"agent":"agent-03","task":"T1"}`); rec.Code != 403 {
		t.Errorf("member with the full token assigned: %d", rec.Code)
	}
}

// A member runs as themselves, however the request names its sender, and
// within their limit; an operator on the tailnet is the operator.
func TestAMemberRunsAsThemselves(t *testing.T) {
	f, h, _ := tailnetSetup(t)
	if rec := tdo(t, h, "POST", "/v1/run", ramIP, "", `{"brief":"fix it","from":"operator"}`); rec.Code != 200 {
		t.Fatalf("run: %d %s", rec.Code, rec.Body)
	}
	if rec := tdo(t, h, "POST", "/v1/run", opIP, "", `{"brief":"fix it","from":"karmax"}`); rec.Code != 200 {
		t.Fatalf("operator run: %d %s", rec.Code, rec.Body)
	}
	want := `run brief="fix it" repo="" agent="" from="ram@example.com" task="" queue=0s limit=2|` +
		`run brief="fix it" repo="" agent="" from="karmax" task="" queue=0s`
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("calls = %s", got)
	}
}

func TestAMemberSeesAndActsOnTheirOwnTasksOnly(t *testing.T) {
	f, h, _ := tailnetSetup(t)
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/v1/tasks/T-ram", "", 200},
		{"GET", "/v1/tasks/T1", "", 403},
		{"GET", "/v1/tasks/T-missing", "", 404},
		{"POST", "/v1/tasks/T-ram/followup", `{"from":"operator","text":"add a test"}`, 200},
		{"POST", "/v1/tasks/T1/followup", `{"text":"x"}`, 403},
		{"POST", "/v1/tasks/T-ram/cancel", `{"from":"eve@example.com","reason":"done elsewhere"}`, 200},
		{"POST", "/v1/tasks/T1/cancel", `{}`, 403},
		{"POST", "/v1/done", `{"task":"T-ram","agent":"agent-07","outcome":"merged"}`, 200},
		{"POST", "/v1/done", `{"agent":"agent-03"}`, 400},
		{"POST", "/v1/done", `{"task":"T1"}`, 403},
		{"GET", "/v1/wait?task=T-ram&agent=agent-05", "", 200},
		{"GET", "/v1/wait?agent=agent-03", "", 400},
		{"GET", "/v1/wait?task=T1", "", 403},
		{"GET", "/v1/tasks?open=1", "", 200},
		{"GET", "/v1/peek?agent=agent-03", "", 200},
		{"GET", "/v1/peek?agent=agent-01", "", 403},
		{"GET", "/v1/peek?agent=agent-02", "", 403}, // at work on the operator's task
		{"GET", "/v1/status", "", 200},
		{"GET", "/v1/pick", "", 200},
		{"GET", "/v1/guide", "", 200},
		{"GET", "/v1/quota", "", 200},
		{"GET", "/v1", "", 200},
		// The operator's: inbox, events, typing into sessions, the hosts.
		{"GET", "/v1/inbox", "", 403},
		{"GET", "/v1/events", "", 403},
		{"GET", "/v1/history", "", 403},
		{"POST", "/v1/prompt", `{"agent":"agent-03","text":"x"}`, 403},
		{"POST", "/v1/tell", `{"from":"ram@example.com","to":"agent-03","text":"x"}`, 403},
		{"POST", "/v1/assign", `{"agent":"agent-03","task":"T2"}`, 403},
		{"POST", "/v1/rotate", `{"agent":"agent-03"}`, 403},
		{"POST", "/v1/enable", `{"agent":"agent-03"}`, 403},
		{"POST", "/v1/agents/agent-03/model", `{"model":"opus"}`, 403},
		{"GET", "/v1/agents/agent-03/model", ``, 403},
		{"POST", "/v1/run", `{"brief":"x","model":"opus"}`, 403}, // a member does not choose the model
		{"POST", "/v1/up", ``, 403},
		{"DELETE", "/v1/archives?older=1d", ``, 403},
	} {
		if rec := tdo(t, h, c.method, c.path, ramIP, "", c.body); rec.Code != c.want {
			t.Errorf("%s %s: %d (%s), want %d", c.method, c.path, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
	}
	calls := strings.Join(f.calls, "|")
	for _, want := range []string{
		"followup T-ram ram@example.com add a test",
		"cancel T-ram ram@example.com done elsewhere",
		"done  T-ram merged", // by the task: not an agent the member named
		"wait  T-ram 0",
		`tasks open=true agent="" n=0 by=ram@example.com`,
		"peek agent-03",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q in %s", want, calls)
		}
	}
	for _, never := range []string{"T1", "agent-01", "agent-02", "prompt", "assign"} {
		for _, c := range f.calls {
			if strings.Contains(c, never) && !strings.HasPrefix(c, "tasks") {
				t.Errorf("a refused call reached the fleet: %s", c)
			}
		}
	}
	// The operator on the tailnet is not held to anyone's tasks.
	if rec := tdo(t, h, "GET", "/v1/tasks/T1", opIP, "", ""); rec.Code != 200 {
		t.Errorf("operator: %d", rec.Code)
	}
}

// People on the list get fleetctl from fleetd itself: an install script
// that points at the address they used, and the builds.
func TestTheTailnetServesFleetctl(t *testing.T) {
	_, h, cfg := tailnetSetup(t)
	rec := tdo(t, h, "GET", "/install.sh", ramIP, "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "url='http://kali.tailnet-example.ts.net:7878'") {
		t.Fatalf("install.sh: %d %s", rec.Code, rec.Body)
	}
	sh := exec.Command("sh", "-n")
	sh.Stdin = strings.NewReader(rec.Body.String())
	if out, err := sh.CombinedOutput(); err != nil {
		t.Fatalf("install.sh does not parse: %v %s", err, out)
	}
	if rec := tdo(t, h, "GET", "/install.ps1", ramIP, "", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "$url = 'http://kali.tailnet-example.ts.net:7878'") {
		t.Fatalf("install.ps1: %d %s", rec.Code, rec.Body)
	}
	// A Host that is not a host name never reaches the script.
	req := httptest.NewRequest("GET", "/install.sh", nil)
	req.RemoteAddr, req.Host = ramIP+":1", "x';rm -rf ~;'"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "rm -rf") || !strings.Contains(rec.Body.String(), "url='http://100.64.0.1:7878'") {
		t.Fatalf("install.sh for a bad Host: %s", rec.Body)
	}

	if err := os.WriteFile(filepath.Join(cfg.Dist, "fleetctl-darwin-arm64"), []byte("BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rec := tdo(t, h, "GET", "/dl/fleetctl-darwin-arm64", ramIP, "", ""); rec.Code != 200 || rec.Body.String() != "BINARY" {
		t.Fatalf("download: %d %s", rec.Code, rec.Body)
	}
	if rec := tdo(t, h, "GET", "/dl/fleetctl-linux-amd64", ramIP, "", ""); rec.Code != 404 || !strings.Contains(rec.Body.String(), "dist.sh") {
		t.Fatalf("missing build: %d %s", rec.Code, rec.Body)
	}
	for _, p := range []string{"/dl/fleet.env", "/dl/..%2Ffleet.env", "/dl/fleetctl-darwin-arm64.sh"} {
		if rec := tdo(t, h, "GET", p, ramIP, "", ""); rec.Code != 404 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if rec := tdo(t, h, "GET", "/install.sh", outIP, "", ""); rec.Code != 403 {
		t.Errorf("install.sh for someone not on the list: %d", rec.Code)
	}
}

// Inside the API, a tailnet caller's role is the scope, whatever token the
// request also carries.
func TestACallerOutranksAToken(t *testing.T) {
	f, api := setup(t)
	req := httptest.NewRequest("POST", "/v1/assign", strings.NewReader(`{"agent":"agent-03","task":"T2"}`))
	req.Header.Set("Authorization", "Bearer full")
	req = req.WithContext(context.WithValue(req.Context(), callerKey{}, &Caller{Login: "ram@example.com", Role: "member"}))
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != 403 || len(f.calls) != 0 {
		t.Fatalf("member with the full token: %d, calls %v", rec.Code, f.calls)
	}
}

// The install script registers fleetctl's MCP server with Claude Code: once,
// whatever the runs; again when it names another fleetctl; and never another
// server. Without claude it says what to run later. Here curl, fleetctl and
// claude are fakes on PATH, in a home of the test's own.
func TestTheInstallScriptRegistersTheMCPServerOnceAndReplacesAStalePath(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	_, h, _ := tailnetSetup(t)
	tmp := t.TempDir()
	script := filepath.Join(tmp, "install.sh")
	if err := os.WriteFile(script, tdo(t, h, "GET", "/install.sh", ramIP, "", "").Body.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	bin, noClaude, state := filepath.Join(tmp, "fake"), filepath.Join(tmp, "fake-no-claude"), filepath.Join(tmp, "mcp")
	for _, d := range []string{bin, noClaude, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	curl := `#!/bin/sh
while [ $# -gt 0 ]; do case $1 in -o) out=$2; shift ;; esac; shift; done
printf '#!/bin/sh\necho "fleetctl $*" >>"$LOG"\n' >"$out"
`
	// claude keeps each server's command in $STATE/<name>.
	claude := `#!/bin/sh
echo "claude $*" >>"$LOG"
case "$1 $2" in
"mcp get")
	[ -f "$STATE/$3" ] || { echo "No MCP server named \"$3\"." >&2; exit 1; }
	printf '%s:\n  Scope: User config\n  Type: stdio\n  Command: %s\n  Args: mcp\n' "$3" "$(cat "$STATE/$3")" ;;
"mcp remove")
	[ -f "$STATE/$5" ] || exit 1
	rm "$STATE/$5" ;;
"mcp add")
	[ ! -f "$STATE/$5" ] || { echo "MCP server $5 already exists in user config" >&2; exit 1; }
	echo "$7" >"$STATE/$5" ;;
esac
`
	for p, body := range map[string]string{bin + "/curl": curl, noClaude + "/curl": curl, bin + "/claude": claude} {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(tmp, "bin")
	logf := filepath.Join(tmp, "log")
	install := func(path string) (out, log string) {
		t.Helper()
		_ = os.Remove(logf)
		sh := exec.Command("sh", script)
		sh.Env = []string{"PATH=" + path + ":/usr/bin:/bin", "HOME=" + tmp, "FLEETCTL_DIR=" + dir, "LOG=" + logf, "STATE=" + state}
		b, err := sh.CombinedOutput()
		if err != nil {
			t.Fatalf("install.sh: %v\n%s", err, b)
		}
		l, _ := os.ReadFile(logf)
		return string(b), string(l)
	}
	registered := func() string {
		b, _ := os.ReadFile(filepath.Join(state, "fleet"))
		return strings.TrimSpace(string(b))
	}
	_ = os.WriteFile(filepath.Join(state, "other"), []byte("/opt/other\n"), 0o644)
	fleetctl := dir + "/fleetctl"

	out, log := install(bin)
	if !strings.Contains(out, "registered the fleet MCP server with Claude Code: "+fleetctl+" mcp") || registered() != fleetctl {
		t.Fatalf("first install: %s\nlog:\n%s", out, log)
	}
	if !strings.Contains(log, "fleetctl connect http://kali.tailnet-example.ts.net:7878\nfleetctl skill install --force\n") ||
		!strings.Contains(log, "claude mcp add --scope user fleet -- "+fleetctl+" mcp\n") {
		t.Fatalf("first install ran:\n%s", log)
	}

	out, log = install(bin)
	if !strings.Contains(out, "already has the fleet MCP server") || strings.Contains(log, "mcp add") || strings.Contains(log, "mcp remove") {
		t.Fatalf("second install: %s\nlog:\n%s", out, log)
	}

	_ = os.WriteFile(filepath.Join(state, "fleet"), []byte("/old/fleetctl\n"), 0o644)
	out, log = install(bin)
	if !strings.Contains(out, "registered the fleet MCP server") || registered() != fleetctl ||
		!strings.Contains(log, "claude mcp remove --scope user fleet\nclaude mcp add ") {
		t.Fatalf("install over a stale path: %s\nlog:\n%s", out, log)
	}
	if b, _ := os.ReadFile(filepath.Join(state, "other")); string(b) != "/opt/other\n" {
		t.Fatalf("another server changed: %q", b)
	}

	_ = os.Remove(filepath.Join(state, "fleet"))
	out, _ = install(noClaude)
	if !strings.Contains(out, "claude was not found: once Claude Code is installed, run  claude mcp add --scope user fleet -- "+fleetctl+" mcp") ||
		registered() != "" {
		t.Fatalf("install without claude: %s", out)
	}
}

// The Windows script does the same in PowerShell, and quotes the -- so a
// script shim for claude still gets it.
func TestTheWindowsInstallScriptRegistersTheMCPServer(t *testing.T) {
	_, h, _ := tailnetSetup(t)
	ps := tdo(t, h, "GET", "/install.ps1", ramIP, "", "").Body.String()
	for _, want := range []string{
		"& $exe skill install --force\n",
		"if (Get-Command claude -ErrorAction SilentlyContinue) {",
		"$got = (& claude mcp get fleet 2>$null | Out-String)",
		"if ($got -match ('(?m)Command: ' + [regex]::Escape($exe) + '\\s*$')) {",
		"& claude mcp remove --scope user fleet 2>$null | Out-Null\n    & claude mcp add --scope user fleet '--' $exe mcp | Out-Null",
		`Write-Host "claude was not found: once Claude Code is installed, run  $add"`,
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("install.ps1 lacks %q", want)
		}
	}
	if strings.Index(ps, "skill install") > strings.Index(ps, "claude mcp add") {
		t.Error("install.ps1 registers the MCP server before it connects and installs the skills")
	}
}
