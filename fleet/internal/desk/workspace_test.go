package desk

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

var bg = context.Background()

func ptr[T any](v T) *T { return &v }

func (d *testDesk) all() fleetapi.DeskList {
	d.t.Helper()
	l, err := d.List(bg, 0)
	if err != nil {
		d.t.Fatal(err)
	}
	return l
}

func wsNames(l fleetapi.DeskList) []string {
	var out []string
	for _, w := range l.Workspaces {
		out = append(out, w.ID+":"+w.Name+":"+w.Layout)
	}
	return out
}

// inWorkspace is the names of ws's sessions in order; "" is every session's.
func inWorkspace(l fleetapi.DeskList, ws string) []string {
	var out []string
	for _, s := range l.Sessions {
		if ws == "" || s.Workspace == ws {
			out = append(out, s.Name)
		}
	}
	return out
}

// A desk.json from before workspaces: every session lands in Main, and
// the first workspace is where a session asked for none goes.
func TestEverySessionIsInAWorkspace(t *testing.T) {
	d := newTestDesk(t)
	os.MkdirAll(filepath.Dir(d.State), 0o700)
	old := `{"boot_id":"boot-1","sessions":[{"name":"old","dir":"` + d.home + `","command":"shell","order":0}]}`
	os.WriteFile(d.State, []byte(old), 0o600)
	d.restart()
	d.tmux("new-session", "-d", "-s", "old") // it runs
	l := d.all()
	if got := wsNames(l); !slices.Equal(got, []string{"main:Main:tabs"}) || l.Sessions[0].Workspace != "main" {
		t.Fatalf("workspaces %v, sessions %+v", got, l.Sessions)
	}
	w, err := d.CreateWorkspace(bg, fleetapi.DeskWorkspaceCreate{Name: "  Web\nfront  ", Layout: "grid"})
	if err != nil || w.Name != "Web front" || w.Layout != "grid" || w.Order != 1 {
		t.Fatalf("workspace %+v %v", w, err)
	}
	a := d.create(fleetapi.DeskCreate{Dir: "~/code", Name: "a", Command: "shell"})
	b := d.create(fleetapi.DeskCreate{Dir: "~/code", Name: "b", Command: "shell", Workspace: w.ID, Label: "Acme Site"})
	c := d.create(fleetapi.DeskCreate{Dir: "~/code", Name: "c", Command: "shell"})
	if a.Workspace != "main" || b.Workspace != w.ID || b.Label != "Acme Site" {
		t.Fatalf("a %+v, b %+v", a, b)
	}
	l = d.all()
	if got := inWorkspace(l, ""); !slices.Equal(got, []string{"old", "a", "c", "b"}) { // a workspace's sessions stay together
		t.Fatalf("order %v", got)
	}
	if _, err := d.Create(bg, fleetapi.DeskCreate{Dir: "~/code", Workspace: "nope"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a session in no such workspace: %v", err)
	}
	_ = c
	d.restart() // what is saved says the same
	if got := inWorkspace(d.all(), ""); !slices.Equal(got, []string{"old", "a", "c", "b"}) {
		t.Fatalf("after a restart: %v", got)
	}
}

func TestSessionsMoveBetweenWorkspacesAndWithin(t *testing.T) {
	d := newTestDesk(t)
	w, _ := d.CreateWorkspace(bg, fleetapi.DeskWorkspaceCreate{Name: "Two"})
	for _, n := range []string{"a", "b", "c"} {
		d.create(fleetapi.DeskCreate{Dir: "~", Name: n, Command: "shell"})
	}
	d.create(fleetapi.DeskCreate{Dir: "~", Name: "x", Command: "shell", Workspace: w.ID})
	move := func(name string, u fleetapi.DeskUpdate) error {
		_, err := d.Update(bg, name, u)
		return err
	}
	if err := move("c", fleetapi.DeskUpdate{Before: ptr("a")}); err != nil {
		t.Fatal(err)
	}
	if got := inWorkspace(d.all(), "main"); !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Fatalf("c in front of a: %v", got)
	}
	if err := move("c", fleetapi.DeskUpdate{Before: ptr("")}); err != nil {
		t.Fatal(err)
	}
	if got := inWorkspace(d.all(), "main"); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("c to the end: %v", got)
	}
	if err := move("a", fleetapi.DeskUpdate{Workspace: ptr(w.ID), Label: ptr(" API ")}); err != nil {
		t.Fatal(err)
	}
	l := d.all()
	if got := inWorkspace(l, w.ID); !slices.Equal(got, []string{"x", "a"}) || l.Sessions[slices.IndexFunc(l.Sessions, func(s fleetapi.DeskSession) bool { return s.Name == "a" })].Label != "API" {
		t.Fatalf("a moved to %s: %v %+v", w.ID, got, l.Sessions)
	}
	if err := move("b", fleetapi.DeskUpdate{Workspace: ptr(w.ID), Before: ptr("x")}); err != nil {
		t.Fatal(err)
	}
	if got := inWorkspace(d.all(), w.ID); !slices.Equal(got, []string{"b", "x", "a"}) {
		t.Fatalf("b in front of x: %v", got)
	}
	before := inWorkspace(d.all(), "")
	for _, u := range []fleetapi.DeskUpdate{{Before: ptr("c")}, {Workspace: ptr("nope")}, {Before: ptr("b")}} {
		if err := move("b", u); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v, want refused", u, err)
		}
	}
	if err := move("nope", fleetapi.DeskUpdate{Label: ptr("x")}); !errors.Is(err, ErrNoSession) {
		t.Errorf("no such session: %v", err)
	}
	if got := inWorkspace(d.all(), ""); !slices.Equal(got, before) {
		t.Fatalf("a refused move moved: %v, was %v", got, before)
	}
}

func TestWorkspacesAreRenamedMovedAndRemoved(t *testing.T) {
	d := newTestDesk(t)
	a, _ := d.CreateWorkspace(bg, fleetapi.DeskWorkspaceCreate{})
	b, _ := d.CreateWorkspace(bg, fleetapi.DeskWorkspaceCreate{Name: "B", Layout: "main"})
	if a.Name != "Workspace 2" || a.Layout != "tabs" {
		t.Fatalf("a default workspace: %+v", a)
	}
	if _, err := d.CreateWorkspace(bg, fleetapi.DeskWorkspaceCreate{Layout: "spiral"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a layout there is none of: %v", err)
	}
	if _, err := d.UpdateWorkspace(bg, b.ID, fleetapi.DeskWorkspaceUpdate{Name: ptr("Bee"), Layout: ptr("columns"), Before: ptr("main")}); err != nil {
		t.Fatal(err)
	}
	if got := wsNames(d.all()); !slices.Equal(got, []string{b.ID + ":Bee:columns", "main:Main:tabs", a.ID + ":Workspace 2:tabs"}) {
		t.Fatalf("after the move: %v", got)
	}
	for _, u := range []fleetapi.DeskWorkspaceUpdate{{Name: ptr(" ")}, {Layout: ptr("x")}, {Before: ptr(b.ID)}, {Before: ptr("nope")}} {
		if _, err := d.UpdateWorkspace(bg, b.ID, u); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v, want refused", u, err)
		}
	}
	d.create(fleetapi.DeskCreate{Dir: "~", Name: "in-b", Command: "shell", Workspace: b.ID})
	if err := d.DeleteWorkspace(bg, b.ID, false); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("a workspace with a session: %v", err)
	}
	if err := d.DeleteWorkspace(bg, b.ID, true); err != nil {
		t.Fatal(err)
	}
	if out := d.tmux("has-session", "-t", "=in-b"); !strings.Contains(out, "can't find") && !strings.Contains(out, "no server") {
		t.Fatalf("its session runs on: %q", out)
	}
	if err := d.DeleteWorkspace(bg, a.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteWorkspace(bg, "main", false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("the last workspace: %v", err)
	}
	if err := d.DeleteWorkspace(bg, "nope", false); !errors.Is(err, ErrNoSession) {
		t.Fatalf("no such workspace: %v", err)
	}
	if l := d.all(); len(l.Workspaces) != 1 || len(l.Sessions) != 0 {
		t.Fatalf("left %+v", l)
	}
}

// What the sidebar shows of a session: Claude Code's state, the title it
// gave the terminal without its spinner, where the pane is and its branch.
func TestTheListSaysWhatEachSessionIsDoing(t *testing.T) {
	d := newTestDesk(t)
	repo := filepath.Join(d.home, "code", "api")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/feature/x\n"), 0o644)
	s := d.create(fleetapi.DeskCreate{Dir: "~/code/api"})
	d.tmux("select-pane", "-t", "="+s.Name+":", "-T", "✳ Fix the build")
	var got fleetapi.DeskSession
	eventually(t, "claude's state", func() bool {
		got = d.all().Sessions[0]
		return got.Status == fleetapi.StatusIdle
	})
	if got.Title != "Fix the build" || got.Cwd != repo || got.Branch != "feature/x" {
		t.Fatalf("listed %+v", got)
	}
	sh := d.create(fleetapi.DeskCreate{Dir: "~", Name: "sh", Command: "shell"})
	for _, x := range d.all().Sessions {
		if x.Name == sh.Name && (x.Status != "" || x.Branch != "") {
			t.Fatalf("a shell: %+v", x)
		}
	}
}

func TestCleanTitle(t *testing.T) {
	for in, want := range map[string]string{
		"✳ Fix the build":   "Fix the build",
		"⠂ Scanning for x":  "Scanning for x",
		"kali":              "",
		"kali.example.net":  "",
		"":                  "",
		"Acme Site":       "Acme Site",
		"· 2 tasks\tleft\n": "2 tasks left",
	} {
		if got := cleanTitle(in, "kali.example.net"); got != want {
			t.Errorf("cleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// The long poll answers a change at once, and nothing until then; however
// many windows wait, tmux is asked once a SnapEvery.
func TestALongPollWaitsForAChange(t *testing.T) {
	d := newTestDesk(t)
	calls := filepath.Join(t.TempDir(), "calls")
	real, _ := exec.LookPath("tmux")
	wrap := filepath.Join(t.TempDir(), "tmux")
	os.WriteFile(wrap, []byte("#!/bin/sh\necho \"$*\" >> '"+calls+"'\nexec '"+real+"' \"$@\"\n"), 0o755)
	d.Tmux = wrap
	d.create(fleetapi.DeskCreate{Dir: "~", Name: "a", Command: "shell"})
	first, err := d.Changes(bg, "", time.Minute)
	if err != nil || first.Version == "" {
		t.Fatalf("first: %+v %v", first, err)
	}
	start := time.Now()
	same, _ := d.Changes(bg, first.Version, 1500*time.Millisecond)
	if el := time.Since(start); same.Version != first.Version || el < 1400*time.Millisecond {
		t.Fatalf("no change: answered after %s with %s", el, same.Version)
	}
	done := make(chan fleetapi.DeskList)
	go func() {
		l, _ := d.Changes(bg, first.Version, 30*time.Second)
		done <- l
	}()
	time.Sleep(200 * time.Millisecond)
	start = time.Now()
	d.Update(bg, "a", fleetapi.DeskUpdate{Label: ptr("renamed")})
	select {
	case l := <-done:
		if l.Version == first.Version || l.Sessions[0].Label != "renamed" || time.Since(start) > time.Second {
			t.Fatalf("after the change: %+v in %s", l, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the change was not answered")
	}
	os.Remove(calls)
	time.Sleep(SnapEvery)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); d.Changes(bg, "", 0) }()
	}
	wg.Wait()
	b, _ := os.ReadFile(calls)
	if n := strings.Count(string(b), "list-sessions"); n != 1 {
		t.Fatalf("20 windows asked tmux %d times:\n%s", n, b)
	}
}

func TestServerOptionsAreSet(t *testing.T) {
	d := newTestDesk(t)
	d.create(fleetapi.DeskCreate{Dir: "~", Name: "a", Command: "shell"})
	for _, c := range [][]string{{"-gv", "mouse", "on"}, {"-gv", "status", "off"}, {"-sv", "escape-time", "10"}, {"-sv", "set-clipboard", "on"}} {
		if got := d.tmux("show", c[0], c[1]); got != c[2] {
			t.Errorf("%s = %q, want %q", c[1], got, c[2])
		}
	}
}

func TestAPasteKeepsItsName(t *testing.T) {
	d := newTestDesk(t)
	d.PasteDir = t.TempDir()
	d.create(fleetapi.DeskCreate{Dir: "~", Name: "a", Command: "shell"})
	p, err := d.Paste(bg, "a", "Screen Shot (1).png", false, strings.NewReader("PNG"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p.Path) != "Screen-Shot-1.png" || !strings.HasPrefix(p.Path, d.PasteDir+"/") || p.Size != 3 {
		t.Fatalf("pasted %+v", p)
	}
	if b, _ := os.ReadFile(p.Path); string(b) != "PNG" {
		t.Fatalf("content %q", b)
	}
	q, _ := d.Paste(bg, "a", "Screen Shot (1).png", false, strings.NewReader("two"))
	if q.Path == p.Path {
		t.Fatal("a second paste of the same name overwrote the first")
	}
	for in, want := range map[string]string{"../../etc/passwd": "passwd", `C:\x\r$(id).txt`: "r-id.txt", "": "pasted", "..": "pasted", "a'b\"c;d|e.go": "a-b-c-d-e.go"} {
		if got := pasteName(in); got != want {
			t.Errorf("pasteName(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := d.Paste(bg, "nope", "x", false, strings.NewReader("x")); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a paste for no session: %v", err)
	}
}

func tarOf(t *testing.T, gz bool, entries ...*tar.Header) []byte {
	var buf bytes.Buffer
	var w *tar.Writer
	var z *gzip.Writer
	if gz {
		z = gzip.NewWriter(&buf)
		w = tar.NewWriter(z)
	} else {
		w = tar.NewWriter(&buf)
	}
	for _, h := range entries {
		body := h.Linkname
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
			h.Linkname = ""
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			w.Write([]byte(body))
		}
	}
	w.Close()
	if z != nil {
		z.Close()
	}
	return buf.Bytes()
}

// A folder pasted is its directories and files; links, and anything that
// would land outside it, are not written.
func TestAPastedFolderIsUnpackedInsideItself(t *testing.T) {
	d := newTestDesk(t)
	d.PasteDir = t.TempDir()
	d.create(fleetapi.DeskCreate{Dir: "~", Name: "a", Command: "shell"})
	for _, gz := range []bool{false, true} {
		body := tarOf(t, gz,
			&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
			&tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Linkname: "one"},
			&tar.Header{Name: "sub/b.txt", Typeflag: tar.TypeReg, Mode: 0o755, Linkname: "two"},
			&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		)
		p, err := d.Paste(bg, "a", "My Project", true, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(p.Path) != "My-Project" || p.Size != 6 {
			t.Fatalf("pasted %+v", p)
		}
		a, _ := os.ReadFile(filepath.Join(p.Path, "a.txt"))
		b, _ := os.ReadFile(filepath.Join(p.Path, "sub", "b.txt"))
		if string(a) != "one" || string(b) != "two" {
			t.Fatalf("gz %v: %q %q", gz, a, b)
		}
		if _, err := os.Lstat(filepath.Join(p.Path, "link")); !os.IsNotExist(err) {
			t.Fatalf("a link was written: %v", err)
		}
	}
	outside := filepath.Join(filepath.Dir(d.PasteDir), "escaped.txt")
	for _, name := range []string{"../escaped.txt", "../../escaped.txt", "/tmp/escaped.txt"} {
		body := tarOf(t, false, &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Linkname: "x"})
		if _, err := d.Paste(bg, "a", "evil", true, bytes.NewReader(body)); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("a file landed outside the paste")
	}
	days, _ := os.ReadDir(d.PasteDir)
	for _, day := range days {
		left, _ := os.ReadDir(filepath.Join(d.PasteDir, day.Name()))
		if len(left) != 2 { // the two good folders; the refused ones are removed
			t.Fatalf("left %d pastes", len(left))
		}
	}
}

func TestOldPastesAreSwept(t *testing.T) {
	d := newTestDesk(t)
	d.PasteDir = t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.Local)
	d.Now = func() time.Time { return now }
	for _, day := range []string{"2026-05-10", "2026-05-12", "2026-05-14", "2026-05-20", "notes"} {
		os.MkdirAll(filepath.Join(d.PasteDir, day, "x"), 0o700)
	}
	d.sweep()
	left, _ := os.ReadDir(d.PasteDir)
	var got []string
	for _, e := range left {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, []string{"2026-05-14", "2026-05-20", "notes"}) {
		t.Fatalf("left %v", got)
	}
}

func TestAFileIsARegularOneAtAnAbsolutePath(t *testing.T) {
	d := newTestDesk(t)
	f := filepath.Join(d.home, "report.txt")
	os.WriteFile(f, []byte("hi"), 0o600)
	h, fi, err := d.File("~/report.txt")
	if err != nil || fi.Size() != 2 {
		t.Fatalf("%v %v", fi, err)
	}
	h.Close()
	for p, want := range map[string]error{d.home: ErrInvalid, "~/nope": ErrNoSession, "/dev/null": ErrInvalid} {
		if _, _, err := d.File(p); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", p, err, want)
		}
	}
}

// The list a window is sent says nothing of screens, and stays the same
// version while nothing changes.
func TestTheVersionIsStable(t *testing.T) {
	d := newTestDesk(t)
	d.create(fleetapi.DeskCreate{Dir: "~", Name: "a", Command: "shell"})
	a, _ := d.Changes(bg, "", 0)
	time.Sleep(SnapEvery + 100*time.Millisecond)
	b, _ := d.Changes(bg, "", 0)
	if a.Version != b.Version {
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		t.Fatalf("nothing changed, the version did:\n%s\n%s", ja, jb)
	}
}
