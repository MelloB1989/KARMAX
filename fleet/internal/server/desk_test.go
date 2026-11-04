package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/desk"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/term"
)

// fakeDesk is a desk that records what it was asked.
type fakeDesk struct {
	mu    sync.Mutex
	calls []string
}

func (d *fakeDesk) record(format string, a ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, fmt.Sprintf(format, a...))
}

func (d *fakeDesk) asked() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func (d *fakeDesk) List(_ context.Context, lines int) (fleetapi.DeskList, error) {
	d.record("list %d", lines)
	return fleetapi.DeskList{Host: "kali", Sessions: []fleetapi.DeskSession{{Name: "api", Dir: "/home/op/code/api", Command: "claude", Running: true}}}, nil
}

func (d *fakeDesk) Create(_ context.Context, c fleetapi.DeskCreate) (fleetapi.DeskSession, error) {
	d.record("create %s %s %s", c.Name, c.Dir, c.Command)
	if c.Dir == "/nowhere" {
		return fleetapi.DeskSession{}, fmt.Errorf("dir /nowhere: no such directory: %w", desk.ErrInvalid)
	}
	return fleetapi.DeskSession{Name: "api", Dir: c.Dir, Command: "claude", Running: true}, nil
}

func (d *fakeDesk) End(_ context.Context, name string) error {
	d.record("end %s", name)
	if name != "api" {
		return fmt.Errorf("%w %q", desk.ErrNoSession, name)
	}
	return nil
}

func (d *fakeDesk) Changes(_ context.Context, since string, wait time.Duration) (fleetapi.DeskList, error) {
	d.record("changes %s %s", since, wait)
	return fleetapi.DeskList{Host: "kali", Version: "v2", Workspaces: []fleetapi.DeskWorkspace{{ID: "main", Name: "Main", Layout: "tabs"}},
		Sessions: []fleetapi.DeskSession{{Name: "api", Workspace: "main", Running: true, Status: fleetapi.StatusIdle}}}, nil
}

func (d *fakeDesk) Update(_ context.Context, name string, u fleetapi.DeskUpdate) (fleetapi.DeskSession, error) {
	b, _ := json.Marshal(u)
	d.record("update %s %s", name, b)
	if name != "api" {
		return fleetapi.DeskSession{}, fmt.Errorf("%w %q", desk.ErrNoSession, name)
	}
	return fleetapi.DeskSession{Name: name}, nil
}

func (d *fakeDesk) CreateWorkspace(_ context.Context, c fleetapi.DeskWorkspaceCreate) (fleetapi.DeskWorkspace, error) {
	d.record("workspace %s %s", c.Name, c.Layout)
	if c.Layout == "spiral" {
		return fleetapi.DeskWorkspace{}, fmt.Errorf("layout spiral: %w", desk.ErrInvalid)
	}
	return fleetapi.DeskWorkspace{ID: "w1", Name: c.Name, Layout: c.Layout}, nil
}

func (d *fakeDesk) UpdateWorkspace(_ context.Context, id string, u fleetapi.DeskWorkspaceUpdate) (fleetapi.DeskWorkspace, error) {
	b, _ := json.Marshal(u)
	d.record("workspace-update %s %s", id, b)
	return fleetapi.DeskWorkspace{ID: id}, nil
}

func (d *fakeDesk) DeleteWorkspace(_ context.Context, id string, end bool) error {
	d.record("workspace-delete %s %v", id, end)
	if id == "busy" && !end {
		return fmt.Errorf("%w: busy has 2 sessions", desk.ErrNotEmpty)
	}
	return nil
}

func (d *fakeDesk) Paste(_ context.Context, session, file string, tarred bool, body io.Reader) (fleetapi.DeskPasted, error) {
	b, _ := io.ReadAll(body)
	d.record("paste %s %s %v %q", session, file, tarred, b)
	return fleetapi.DeskPasted{Path: "/home/op/.paste/" + file, Size: int64(len(b))}, nil
}

func (d *fakeDesk) File(path string) (*os.File, os.FileInfo, error) {
	d.record("file %s", path)
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: no file", desk.ErrNoSession)
	}
	fi, _ := f.Stat()
	return f, fi, nil
}

func (d *fakeDesk) Dirs(prefix string) []string {
	d.record("dirs %s", prefix)
	return []string{"/home/op/code"}
}

func (d *fakeDesk) Terminal(name string) (term.Target, bool) {
	d.record("terminal %s", name)
	return term.Target{}, false
}

func deskSetup(t *testing.T) (*fakeDesk, http.Handler, http.Handler) {
	t.Helper()
	f, d := &fake{}, &fakeDesk{}
	api := Handler(WithDesk(f, d), Tokens{Full: "full", Relay: "relay", Console: "console"}, nil)
	tn := Tailnet(api, tailnetWho, tagDevices(t), &config.Tailnet{Listen: "100.64.0.1:7878", GuestOpen: 2, Dist: t.TempDir()})
	return d, api, tn
}

func TestTheDeskIsTheOperatorsAlone(t *testing.T) {
	d, api, tn := deskSetup(t)
	routes := []struct{ method, path, body string }{
		{"GET", "/v1/desk?lines=5", ""},
		{"POST", "/v1/desk", `{"dir":"/home/op/code"}`},
		{"DELETE", "/v1/desk/api", ""},
		{"GET", "/v1/desk/dirs?prefix=~/co", ""},
		{"GET", "/v1/desk?since=v1&wait=1", ""},
		{"PATCH", "/v1/desk/api", `{"label":"API"}`},
		{"POST", "/v1/desk/workspaces", `{"name":"Web"}`},
		{"PATCH", "/v1/desk/workspaces/main", `{"layout":"grid"}`},
		{"DELETE", "/v1/desk/workspaces/w1", ""},
		{"POST", "/v1/desk/paste?session=api&file=shot.png", "PNG"},
	}
	for _, r := range routes {
		for tok, want := range map[string]int{"relay": 403, "console": 403, "": 401} {
			if rec := do(t, api, r.method, r.path, tok, r.body); rec.Code != want {
				t.Errorf("%s %s as %q: %d %s, want %d", r.method, r.path, tok, rec.Code, rec.Body, want)
			}
		}
		for _, ip := range []string{ramIP, friendIP} { // guests, whatever they are lent
			if rec := tdo(t, tn, r.method, r.path, ip, "", r.body); rec.Code != 403 {
				t.Errorf("%s %s as a guest: %d %s, want 403", r.method, r.path, rec.Code, rec.Body)
			}
		}
	}
	if c := d.asked(); len(c) != 0 {
		t.Fatalf("refused callers reached the desk: %v", c)
	}
	for _, r := range routes {
		if rec := do(t, api, r.method, r.path, "full", r.body); rec.Code != 200 {
			t.Errorf("%s %s as the operator: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
		if rec := tdo(t, tn, r.method, r.path, opIP, "", r.body); rec.Code != 200 {
			t.Errorf("%s %s from an admin device: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
	}
}

func TestTheDesksRoutes(t *testing.T) {
	d, api, _ := deskSetup(t)
	rec := do(t, api, "GET", "/v1/desk?lines=9999", "full", "")
	var l fleetapi.DeskList
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &l) != nil || l.Host != "kali" || len(l.Sessions) != 1 || l.Sessions[0].Name != "api" {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, api, "POST", "/v1/desk", "full", `{"name":"x","dir":"/home/op/code","command":"claude --model opus"}`)
	var s fleetapi.DeskSession
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &s) != nil || s.Name != "api" || !s.Running {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/v1/desk", `{"dir":"/nowhere"}`, 400},
		{"POST", "/v1/desk", `{"dir":`, 400},
		{"DELETE", "/v1/desk/nope", "", 404},
		{"GET", "/v1/desk?lines=x", "", 400},
	} {
		if rec := do(t, api, c.method, c.path, "full", c.body); rec.Code != c.want {
			t.Errorf("%s %s %s: %d %s, want %d", c.method, c.path, c.body, rec.Code, rec.Body, c.want)
		}
	}
	rec = do(t, api, "GET", "/v1/desk/dirs?prefix="+"~%2Fco", "full", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"dirs":["/home/op/code"]}` {
		t.Fatalf("dirs: %d %s", rec.Code, rec.Body)
	}
	want := []string{"list 500", "create x /home/op/code claude --model opus", "create  /nowhere ", "end nope", "dirs ~/co"}
	if got := d.asked(); !slices.Equal(got, want) {
		t.Fatalf("asked %q, want %q", got, want)
	}
}

// The window's routes: the long poll, workspaces, moves, pastes and
// downloads, each answering the desk's errors as the API does.
func TestTheDesksWorkspaceAndPasteRoutes(t *testing.T) {
	d, api, _ := deskSetup(t)
	rec := do(t, api, "GET", "/v1/desk?since=abc&wait=999", "full", "")
	var l fleetapi.DeskList
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &l) != nil || l.Version != "v2" || len(l.Workspaces) != 1 || l.Sessions[0].Workspace != "main" {
		t.Fatalf("long poll: %d %s", rec.Code, rec.Body)
	}
	file := filepath.Join(t.TempDir(), "report.txt")
	os.WriteFile(file, []byte("hello"), 0o600)
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"PATCH", "/v1/desk/api", `{"workspace":"w1","before":""}`, 200},
		{"PATCH", "/v1/desk/nope", `{"label":"x"}`, 404},
		{"POST", "/v1/desk/workspaces", `{"name":"Web","layout":"spiral"}`, 400},
		{"DELETE", "/v1/desk/workspaces/busy", "", 409},
		{"DELETE", "/v1/desk/workspaces/busy?end=1", "", 200},
		{"POST", "/v1/desk/paste?session=api&file=a.tgz&kind=tar", "TAR", 200},
		{"POST", "/v1/desk/paste?session=api", "x", 400},
		{"POST", "/v1/desk/paste?session=api&file=x&kind=zip", "x", 400},
		{"GET", "/v1/desk/file?path=/nowhere/x", "", 404},
	} {
		if rec := do(t, api, c.method, c.path, "full", c.body); rec.Code != c.want {
			t.Errorf("%s %s %s: %d %s, want %d", c.method, c.path, c.body, rec.Code, rec.Body, c.want)
		}
	}
	rec = do(t, api, "GET", "/v1/desk/file?path="+file, "full", "")
	if rec.Code != 200 || rec.Body.String() != "hello" || !strings.Contains(rec.Header().Get("Content-Disposition"), `filename=report.txt`) {
		t.Fatalf("download: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	want := []string{"changes abc 50s", `update api {"workspace":"w1","before":""}`, `update nope {"label":"x"}`, "workspace Web spiral",
		"workspace-delete busy false", "workspace-delete busy true", `paste api a.tgz true "TAR"`, "file /nowhere/x", "file " + file}
	if got := d.asked(); !slices.Equal(got, want) {
		t.Fatalf("asked %q, want %q", got, want)
	}
}

// A page in the operator's browser reaches fleetd as them (the tailnet
// names the device, not the program): any request a browser marks as a
// page's is refused before it reaches anything.
func TestAWebPageCannotActAsTheOperator(t *testing.T) {
	d, _, tn := deskSetup(t)
	for _, h := range []map[string]string{
		{"Origin": "https://evil.example"},
		{"Origin": "http://100.64.0.1:7878"}, // a page rebound to fleetd's own address
		{"Origin": "null"},
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-origin"},
	} {
		req := httptest.NewRequest("POST", "/v1/desk", strings.NewReader(`{"dir":"/tmp","command":"curl x | sh"}`))
		req.RemoteAddr = net.JoinHostPort(opIP, "40001")
		req.Host = "100.64.0.1:7878"
		for k, v := range h {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		tn.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("%v: %d %s, want 403", h, rec.Code, rec.Body)
		}
	}
	if c := d.asked(); len(c) != 0 {
		t.Fatalf("a page's request reached the desk: %v", c)
	}
}

func TestNoDeskIsNotFound(t *testing.T) {
	_, api := setup(t)
	rec := do(t, api, "GET", "/v1/desk", "full", "")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "desk is off") {
		t.Fatalf("no desk: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, api, "GET", "/v1/term/desk:api", "full", ""); rec.Code != 404 {
		t.Fatalf("a desk terminal with no desk: %d %s", rec.Code, rec.Body)
	}
}

// A desk terminal is the operator's alone; anyone else is told no more
// than about an agent not theirs.
func TestADeskTerminalIsTheOperatorsAlone(t *testing.T) {
	d, api, tn := deskSetup(t)
	agent := tdo(t, tn, "GET", "/v1/term/agent-02?mode=watch", ramIP, "", "") // not lent to ram
	rec := tdo(t, tn, "GET", "/v1/term/desk:api?mode=watch", ramIP, "", "")
	if rec.Code != 403 || agent.Code != 403 || strings.ReplaceAll(rec.Body.String(), "desk:api", "X") != strings.ReplaceAll(agent.Body.String(), "agent-02", "X") {
		t.Fatalf("a guest's desk terminal: %d %s; an agent not theirs: %d %s", rec.Code, rec.Body, agent.Code, agent.Body)
	}
	for _, tok := range []string{"relay", "console"} {
		if rec := do(t, api, "GET", "/v1/term/desk:api?mode=watch", tok, ""); rec.Code != 403 {
			t.Errorf("as %s: %d %s", tok, rec.Code, rec.Body)
		}
	}
	if c := d.asked(); len(c) != 0 {
		t.Fatalf("refused callers reached the desk: %v", c)
	}
	if rec := do(t, api, "GET", "/v1/term/desk:nope?mode=watch", "full", ""); rec.Code != 404 {
		t.Fatalf("no such desk session: %d %s", rec.Code, rec.Body)
	}
	// The console's screens never show a desk session.
	s := screensOf(t, api, "/v1/screens?workers=desk:api", "console")
	if len(s.Screens) != 1 || s.Screens[0].Text != "" || s.Screens[0].Error == "" {
		t.Fatalf("the console's screens: %+v", s.Screens)
	}
}

// fleetctl ssh desk:<name>: the session's tmux on this machine, through the
// same route; nothing resizes its window but the typist's own terminal,
// and the client is gone when the viewer leaves.
func TestATerminalOnADeskSession(t *testing.T) {
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no tmux")
	}
	tmp, err := os.MkdirTemp("", "dk")
	if err != nil {
		t.Fatal(err)
	}
	if len(tmp) > 60 {
		os.RemoveAll(tmp)
		tmp, _ = os.MkdirTemp("/tmp", "dk")
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	t.Setenv("TMUX_TMPDIR", tmp)
	// A tmux that logs each call, in front of the real one.
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "tmux.log")
	os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\necho \"$*\" >> '"+log+"'\nexec '"+real+"' \"$@\"\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	home := t.TempDir()
	m := desk.New("dt-"+newTermID(), t.TempDir())
	m.Tmux, m.Conf, m.Shell, m.Home, m.SystemdRun, m.Host = real, "/dev/null", "/bin/sh", home, "", "kali"
	m.ClaudeDir = filepath.Join(home, ".claude")
	m.BootID = func() string { return "boot-1" }
	m.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "TMUX_TMPDIR=" + tmp, "LANG=C.UTF-8"}
	t.Cleanup(func() { exec.Command(real, "-L", m.Socket, "kill-server").Run() })
	if _, err := m.Create(context.Background(), fleetapi.DeskCreate{Name: "api", Dir: home, Command: "shell"}); err != nil {
		t.Fatal(err)
	}
	termFake.mu.Lock()
	termFake.events = nil
	termFake.mu.Unlock()
	srv := httptest.NewServer(Handler(WithDesk(&fake{}, m), Tokens{Full: "full"}, nil))
	t.Cleanup(srv.Close)

	s, _, err := dialTerm(t, srv, "/v1/term/desk:api?mode=type&cols=100&rows=30", "full")
	if err != nil {
		t.Fatal(err)
	}
	s.until("hello", frameOf(fleetapi.FrameHello))
	s.send([]byte("echo typed-$((40+2))\r"))
	s.until("the echo", func(out string, _ []fleetapi.TermFrame) bool { return strings.Contains(out, "typed-42") })
	clients := func() string {
		out, _ := exec.Command(real, "-L", m.Socket, "list-sessions", "-F", "#{session_attached} #{pane_width}").Output()
		return strings.TrimSpace(string(out))
	}
	if got := clients(); got != "1 100" {
		t.Fatalf("while typing: clients and width %q, want 1 100", got)
	}
	s.c.Close(websocket.StatusNormalClosure, "")
	ev := waitEvents(t, 2)
	if len(ev) != 2 || ev[0].Worker != "desk:api" || ev[1].Phase != "end" || ev[1].Mode != fleetapi.TermType {
		t.Fatalf("events %+v", ev)
	}
	deadline := time.Now().Add(10 * time.Second)
	for clients() != "0 100" {
		if time.Now().After(deadline) {
			t.Fatalf("after leaving: %q, want the session, no client, the typist's width", clients())
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(log)
	calls := strings.TrimSpace(string(b))
	if !strings.Contains(calls, "-L "+m.Socket+" attach -t =api") || strings.Contains(calls, "resize-window") {
		t.Fatalf("tmux calls:\n%s", calls)
	}

	// A watcher's keys reach nothing.
	w, _, err := dialTerm(t, srv, "/v1/term/desk:api?mode=watch&cols=80&rows=24", "full")
	if err != nil {
		t.Fatal(err)
	}
	w.until("the screen", func(out string, _ []fleetapi.TermFrame) bool { return strings.Contains(out, "typed-42") })
	w.send([]byte("echo watched-$((1+1))\r"))
	time.Sleep(300 * time.Millisecond)
	w.c.Close(websocket.StatusNormalClosure, "")
	waitEvents(t, 4)
	out, _ := exec.Command(real, "-L", m.Socket, "capture-pane", "-p", "-t", "=api:").Output()
	if strings.Contains(string(out), "watched-2") {
		t.Fatalf("a watcher typed:\n%s", out)
	}
}
