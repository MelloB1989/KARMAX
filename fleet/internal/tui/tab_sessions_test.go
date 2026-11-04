package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/clipboard"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Tests of the Sessions tab on the Fake's demo desk: Main (api, at work;
// web, waiting) and Ops (infra, a shell; notes, idle), on devbox.

// goTab shows the tab titled title, as its number key does.
func goTab(t *testing.T, a *App, title string) tea.Cmd {
	t.Helper()
	for i, tb := range a.visible() {
		if tb.Title() == title {
			_, cmd := a.Update(keyMsg(fmt.Sprint(i + 1)))
			return cmd
		}
	}
	t.Fatalf("no %s tab", title)
	return nil
}

// runCmd runs cmd and every command it batches, each for a moment: one
// that waits (for output, a tick) is dropped.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(300 * time.Millisecond):
		return nil
	}
	if b, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range b {
			out = append(out, runCmd(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

type deskRig struct {
	t    *testing.T
	c    *Ctx
	f    *fleetapi.Fake
	d    *deskTab
	w, h int
}

func newRig(t *testing.T, w, h int) *deskRig {
	t.Helper()
	c, f := tabCtx(t, fleetapi.RoleAdmin)
	r := &deskRig{t: t, c: c, f: f, d: newSessions().(*deskTab), w: w, h: h}
	r.send(tea.WindowSizeMsg{Width: w, Height: h})
	r.send(FrameMsg{})
	if r.d.list == nil {
		t.Fatal("the desk did not arrive")
	}
	r.view()
	return r
}

// send gives the tab msg and the messages its commands make, in turn.
func (r *deskRig) send(msg tea.Msg) {
	q := []tea.Msg{msg}
	for i := 0; i < len(q) && i < 50; i++ {
		if _, paint := q[i].(deskPaintMsg); paint && i > 0 {
			continue
		}
		q = append(q, runCmd(r.d.Update(q[i], r.c))...)
	}
}

func (r *deskRig) press(keys ...string) {
	for _, k := range keys {
		r.send(deskKey(k))
	}
}

func deskKey(k string) tea.KeyPressMsg {
	switch k {
	case "ctrl+]":
		return tea.KeyPressMsg{Code: ']', Mod: tea.ModCtrl}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+v":
		return tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl}
	case "shift+enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}
	case "super+b":
		return tea.KeyPressMsg{Code: 'b', Mod: tea.ModSuper}
	case "super+1":
		return tea.KeyPressMsg{Code: '1', Mod: tea.ModSuper}
	case "alt+2":
		return tea.KeyPressMsg{Code: '2', Mod: tea.ModAlt}
	}
	return tabKey(k)
}

func (r *deskRig) view() string {
	r.t.Helper()
	v := r.d.View(r.c, r.w, r.h)
	tabSize(r.t, "the desk", v, r.w, r.h)
	return ansi.Strip(v)
}

// term is session name's terminal on the fake, once it is live.
func (r *deskRig) term(name string) *fleetapi.FakeTerm {
	r.t.Helper()
	var ft *fleetapi.FakeTerm
	eventually(r.t, name+"'s terminal", func() bool {
		ft = r.f.DeskTermOf(name)
		t := r.d.term(name)
		if ft == nil || t == nil {
			return false
		}
		st, _, _ := t.status()
		return st == termLive
	})
	return ft
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("%s: not in time", what)
}

func typedIn(ft *fleetapi.FakeTerm, want string) func() bool {
	return func() bool { return strings.Contains(ft.TypedText(), want) }
}

func deskCalls(f *fleetapi.Fake, prefix string) []string {
	var out []string
	for _, c := range slices.Clone(f.Calls) {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestTheDeskShowsItsWorkspacesAndTheShownOnesSessions(t *testing.T) {
	r := newRig(t, 140, 40)
	v := r.view()
	for _, want := range []string{"Main", "Ops", "Fix the cache eviction", "Landing page copy needs", "~/code/api · main", "tabs", "NAV", "devbox"} {
		if !strings.Contains(v, want) {
			t.Errorf("the desk lacks %q:\n%s", want, v)
		}
	}
	r.term("api") // the focused session is live in its pane
	eventually(t, "its screen", func() bool { return strings.Contains(r.view(), "Welcome to Claude Code") })
	for _, sz := range [][2]int{{0, 0}, {1, 1}, {30, 8}, {59, 20}, {60, 20}, {80, 24}, {200, 60}} {
		r.w, r.h = sz[0], sz[1]
		r.send(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		v := r.d.View(r.c, sz[0], sz[1])
		if sz[1] > 0 && sz[0] > 0 {
			tabSize(t, "resized", v, sz[0], sz[1])
		}
	}
}

func TestTypingGoesToTheFocusedSession(t *testing.T) {
	r := newRig(t, 120, 30)
	if r.d.Raw() {
		t.Fatal("the desk opens typing")
	}
	r.press("enter")
	if r.d.mode != typeMode || !r.d.Raw() {
		t.Fatal("⏎ did not type into the session")
	}
	ft := r.term("api")
	r.press("l", "s", "enter", "shift+enter", "ctrl+c")
	eventually(t, "ls typed", typedIn(ft, "ls\r\x1b\r\x03"))
	r.press("ctrl+]")
	if r.d.mode != navMode || r.d.Raw() {
		t.Fatal("Ctrl-] did not step out")
	}
	r.press("q") // the desk's keys now, not the session's
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(ft.TypedText(), "q") {
		t.Fatalf("a key after Ctrl-] reached the session: %q", ft.TypedText())
	}
	if !strings.Contains(r.view(), "TYPING") == (r.d.mode == typeMode) {
		t.Fatal("the head says the wrong mode")
	}
}

// While typing, Ctrl-C and the digits are the session's; stepped out,
// they are the app's again.
func TestTheAppGivesEveryKeyToATypingSession(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	feed := func(cmd tea.Cmd) {
		for _, m := range runCmd(cmd) {
			_, c := a.Update(m)
			runCmd(c)
		}
	}
	s, _ := f.Watch(context.Background(), 0)
	_, cmd := a.Update(SnapshotMsg{Snap: s}) // the desk starts listening with the first snapshot
	feed(cmd)
	feed(goTab(t, a, "Sessions"))
	feed(a.current().Update(FrameMsg{}, &a.c))
	if v := ansi.Strip(view(a)); !strings.Contains(v, "KARMAX ▸ desk") || strings.Contains(v, "╭") && strings.Contains(strings.Split(v, "\n")[0], "╭") {
		t.Fatalf("the desk is not full screen:\n%s", v)
	}
	_, cmd = a.Update(keyMsg("enter"))
	feed(cmd)
	var ft *fleetapi.FakeTerm
	eventually(t, "api's terminal", func() bool { ft = f.DeskTermOf("api"); return ft != nil })
	d := a.current().(*deskTab)
	eventually(t, "live", func() bool { st, _, _ := d.term("api").status(); return st == termLive })
	for _, k := range []string{"ctrl+c", "2", "q", "?"} {
		_, cmd := a.Update(deskKey(k))
		if msgs := runCmd(cmd); slices.ContainsFunc(msgs, func(m tea.Msg) bool { _, q := m.(tea.QuitMsg); return q }) {
			t.Fatalf("%s quit the TUI while typing", k)
		}
	}
	eventually(t, "the keys typed", typedIn(ft, "\x032q?"))
	if a.current().Title() != "Sessions" || a.help {
		t.Fatal("a typed key moved the app")
	}
	a.Update(deskKey("ctrl+]"))
	_, cmd = a.Update(deskKey("ctrl+c"))
	if msgs := runCmd(cmd); !slices.ContainsFunc(msgs, func(m tea.Msg) bool { _, q := m.(tea.QuitMsg); return q }) {
		t.Fatal("stepped out, Ctrl-C does not quit")
	}
}

func TestMovingAroundTheDesk(t *testing.T) {
	r := newRig(t, 140, 40)
	r.press("down", "down", "down") // Main's two sessions, then Ops
	if r.d.active != "wops" {
		t.Fatalf("the cursor on Ops did not show it: %q", r.d.active)
	}
	r.press("super+1")
	if r.d.active != "main" {
		t.Fatal("⌘1 did not show Main")
	}
	r.press("alt+2")
	if r.d.active != "wops" {
		t.Fatal("alt+2 did not show Ops")
	}
	r.press("}")
	if r.d.active != "main" {
		t.Fatal("} did not go round to Main")
	}
	was := r.d.focused()
	r.press("]")
	if r.d.focused() == was || r.d.focused() == "" {
		t.Fatalf("] left the focus on %q", r.d.focused())
	}
	r.press("b")
	if v := r.view(); strings.Contains(v, "KARMAX ▸ desk") {
		t.Fatal("b did not hide the sidebar")
	}
	r.press("super+b")
	if v := r.view(); !strings.Contains(v, "KARMAX ▸ desk") {
		t.Fatal("⌘B did not bring it back")
	}
	r.press("enter")
	r.press("super+b") // ⌘B while typing: the desk's, not the session's
	if r.d.sidebar || r.d.mode != typeMode {
		t.Fatal("⌘B while typing did not hide the sidebar")
	}
}

func TestTheLayoutChangesAtOnceAndOnFleetd(t *testing.T) {
	r := newRig(t, 160, 40)
	r.press("l")
	if w, _ := r.d.ws("main"); w.Layout != fleetapi.LayoutColumns {
		t.Fatalf("layout %s", w.Layout)
	}
	if got := deskCalls(r.f, "desk workspace update main"); len(got) != 1 {
		t.Fatalf("fleetd was told %v", got)
	}
	v := r.view()
	if strings.Count(strings.Split(v, "\n")[1], "~/code/") != 2 {
		t.Fatalf("two panes side by side, each with its head:\n%s", v)
	}
	r.press("z")
	if !strings.Contains(r.view(), "zoomed") {
		t.Fatal("z did not zoom")
	}
}

func TestPaneBoxes(t *testing.T) {
	area := box{10, 1, 101, 40}
	for _, c := range []struct {
		layout string
		n, f   int
		want   []box
	}{
		{"tabs", 3, 1, []box{{}, area, {}}},
		{"columns", 2, 0, []box{{10, 1, 50, 40}, {61, 1, 50, 40}}},
		{"rows", 2, 0, []box{{10, 1, 101, 20}, {10, 21, 101, 20}}},
		{"grid", 3, 0, []box{{10, 1, 50, 20}, {61, 1, 50, 20}, {10, 21, 101, 20}}},
		{"main", 3, 2, []box{{10, 1, 60, 40}, {71, 1, 40, 20}, {71, 21, 40, 20}}},
		{"columns", 6, 4, []box{{}, {}, {}, {}, area, {}}}, // too narrow to split: tabs
	} {
		got := paneBoxes(c.layout, c.n, c.f, area, false)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s ×%d: %v, want %v", c.layout, c.n, got, c.want)
		}
	}
	if got := paneBoxes("grid", 4, 3, area, true); !slices.Equal(got, []box{{}, {}, {}, area}) {
		t.Errorf("zoomed: %v", got)
	}
	g := paneBoxes("grid", 4, 0, area, false)
	if neighbour(g, 0, 1, 0) != 1 || neighbour(g, 0, 0, 1) != 2 || neighbour(g, 3, -1, 0) != 2 || neighbour(g, 0, -1, 0) != -1 {
		t.Errorf("neighbours in %v", g)
	}
}

// fakeClip is a clipboard that holds what a test puts on it.
type fakeClip struct {
	got clipboard.Got
	png []byte
}

func (c *fakeClip) Read(_ context.Context, png string) (clipboard.Got, error) {
	g := c.got
	if g.Image != "" {
		os.WriteFile(png, c.png, 0o600)
		g.Image = png
	}
	return g, nil
}

func (c *fakeClip) PutFile(context.Context, string) error { return nil }

// A file dropped on the terminal (or copied in a file manager) is pasted
// as its path here: it goes to the session's machine, and its path there
// is typed. Other text is typed as it is.
func TestAPastedFileGoesToTheSessionsMachine(t *testing.T) {
	r := newRig(t, 120, 30)
	r.press("enter")
	ft := r.term("api")
	dir := t.TempDir()
	shot := filepath.Join(dir, "shot one.png")
	os.WriteFile(shot, []byte("PNG"), 0o600)
	os.MkdirAll(filepath.Join(dir, "proj", "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "proj", "src", "a.go"), []byte("package a"), 0o600)
	r.send(tea.PasteMsg{Content: strings.ReplaceAll(shot, " ", `\ `) + " '" + filepath.Join(dir, "proj") + "'"})
	eventually(t, "the paths typed", typedIn(ft, `/home/op/.karmax/fleet/desk-paste/shot-one.png /home/op/.karmax/fleet/desk-paste/proj `))
	got := deskCalls(r.f, "desk paste")
	if len(got) != 2 || got[0] != "desk paste api shot one.png tar=false 3" || !strings.HasPrefix(got[1], "desk paste api proj tar=true") {
		t.Fatalf("fleetd took %q", got)
	}
	r.send(tea.PasteMsg{Content: "just words /not/a/file"})
	eventually(t, "text typed", typedIn(ft, "just words /not/a/file"))
	if len(deskCalls(r.f, "desk paste")) != 2 {
		t.Fatal("text was sent as a file")
	}
	r.c.Local = true // on fleetd's own machine the path is already one there: typed as pasted
	r.send(tea.PasteMsg{Content: strings.ReplaceAll(shot, " ", `\ `)})
	eventually(t, "the local path", typedIn(ft, strings.ReplaceAll(shot, " ", `\ `)))
	if len(deskCalls(r.f, "desk paste")) != 2 {
		t.Fatal("a local paste was sent")
	}
}

// Ctrl+V is Claude Code's paste-an-image key: this machine's clipboard,
// files or an image, goes across; with neither the key is typed.
func TestCtrlVSendsTheClipboard(t *testing.T) {
	r := newRig(t, 120, 30)
	clip := &fakeClip{got: clipboard.Got{Image: "x"}, png: []byte("PNGDATA")}
	r.c.Clipboard = clip
	r.press("enter")
	ft := r.term("api")
	r.press("ctrl+v")
	eventually(t, "the image's path", typedIn(ft, "/home/op/.karmax/fleet/desk-paste/clipboard-"))
	if got := deskCalls(r.f, "desk paste api clipboard-"); len(got) != 1 || !strings.HasSuffix(got[0], "tar=false 7") {
		t.Fatalf("fleetd took %q", deskCalls(r.f, "desk paste"))
	}
	f := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(f, []byte("n"), 0o600)
	clip.got = clipboard.Got{Files: []string{f}}
	r.press("ctrl+v")
	eventually(t, "the file's path", typedIn(ft, "/desk-paste/notes.txt "))
	clip.got = clipboard.Got{}
	r.press("ctrl+v")
	eventually(t, "a plain ^V", typedIn(ft, "\x16"))
	r.c.Clipboard = nil
	before := strings.Count(ft.TypedText(), "\x16")
	r.press("ctrl+v")
	eventually(t, "^V with no clipboard here", func() bool { return strings.Count(ft.TypedText(), "\x16") == before+1 })
}

func TestLocalPaths(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a b.png")
	b := filepath.Join(dir, "c'd.txt")
	os.WriteFile(a, nil, 0o600)
	os.WriteFile(b, nil, 0o600)
	home, _ := os.UserHomeDir()
	for in, want := range map[string][]string{
		strings.ReplaceAll(a, " ", `\ `):                               {a},
		`'` + a + `' "` + b + `"`:                                      {a, b},
		strings.ReplaceAll(a, " ", `\ `) + "\n" + `"` + b + `"` + "\n": {a, b},
		"file://" + dir:                                                {dir},
		a + " /no/such/file":                                           nil,
		"relative/path":                                                nil,
		"hello world":                                                  nil,
		`'` + a:                                                        nil, // an unclosed quote
		"~":                                                            {home},
	} {
		if got := localPaths(in); !slices.Equal(got, want) {
			t.Errorf("localPaths(%q) = %q, want %q", in, got, want)
		}
	}
	if got := typedPaths([]string{"/x/a b.png", "/y/c$d"}); got != `/x/a\ b.png /y/c\$d ` {
		t.Errorf("typedPaths = %q", got)
	}
}

// What a program in a session copies (OSC 52, as tmux does from its copy
// mode) lands on this machine's clipboard.
func TestACopyInASessionReachesThisMachine(t *testing.T) {
	m := newTermMgr(context.Background(), &stubClient{conns: make(chan *stubConn)})
	tm := m.get("api", 80, 24)
	tm.emu.Write([]byte("\x1b]52;c;aGVsbG8gZGVzaw==\x07"))
	select {
	case s := <-m.clips:
		if s != "hello desk" {
			t.Fatalf("copied %q", s)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing copied")
	}
	r := newRig(t, 120, 30)
	var kinds []string
	for _, msg := range runCmd(r.d.Update(deskPaintMsg{clips: []string{"hello desk"}}, r.c)) {
		kinds = append(kinds, fmt.Sprintf("%T", msg))
	}
	if !slices.ContainsFunc(kinds, func(k string) bool { return strings.Contains(strings.ToLower(k), "clipboard") }) {
		t.Fatalf("no clipboard command: %v", kinds)
	}
	if _, ok := osc52([]byte("52;c;?")); ok {
		t.Fatal("a query copied")
	}
}

// A session that finishes, or asks, while not on screen is marked, said
// in a toast, and sent to the desktop.
func TestNewsFromASessionNotOnScreen(t *testing.T) {
	r := newRig(t, 140, 40)
	r.f.SetDeskStatus("notes", fleetapi.StatusWorking)
	r.send(FrameMsg{})
	r.f.SetDeskStatus("notes", fleetapi.StatusIdle)
	r.f.SetDeskStatus("api", fleetapi.StatusIdle) // on screen: no news
	r.send(FrameMsg{})
	if r.d.unread["notes"] != fleetapi.NoteDone || r.d.unread["api"] != "" {
		t.Fatalf("unread %v", r.d.unread)
	}
	if !strings.Contains(r.d.toast.Text, "notes finished") {
		t.Fatalf("toast %q", r.d.toast.Text)
	}
	r.f.SetDeskStatus("infra", fleetapi.StatusWaiting)
	r.send(FrameMsg{})
	if r.d.unread["infra"] != fleetapi.NoteNeeds || r.d.WindowTitle() == "" {
		t.Fatalf("waiting: unread %v, title %q", r.d.unread, r.d.WindowTitle())
	}
	r.press("alt+2") // shown: read
	if r.d.unread["notes"] != "" {
		t.Fatalf("shown, still unread: %v", r.d.unread)
	}
}

func TestWorkspacesAreMadeAndSessionsMoved(t *testing.T) {
	r := newRig(t, 140, 40)
	r.press("W")
	for _, ch := range "Web" {
		r.send(tea.KeyPressMsg{Code: ch, Text: string(ch)})
	}
	r.press("enter")
	r.send(FrameMsg{})
	if got := deskCalls(r.f, "desk workspace new"); len(got) != 1 || got[0] != "desk workspace new Web" {
		t.Fatalf("fleetd was asked %v", got)
	}
	if w, _ := r.d.ws(r.d.active); w.Name != "Web" {
		t.Fatalf("shown %q, want the new workspace", w.Name)
	}
	r.press("super+1") // the cursor on Main's focused session, api
	r.press("m", "down", "down", "enter")
	r.send(FrameMsg{})
	if got := deskCalls(r.f, "desk update api"); len(got) != 1 {
		t.Fatalf("fleetd was asked %v", got)
	}
	if s, _ := r.d.session("api"); s.Workspace == "main" {
		t.Fatal("api did not move")
	}
	r.press("x", "n") // asked, kept
	if len(deskCalls(r.f, "desk end")) != 0 {
		t.Fatal("ended without a yes")
	}
}

func TestTheMouseChoosesAndScrolls(t *testing.T) {
	r := newRig(t, 140, 40)
	ft := r.term("api")
	var ops hit
	for _, h := range r.d.hits {
		if h.ws == "wops" {
			ops = h
		}
	}
	r.send(tea.MouseClickMsg{X: ops.b.x + 2, Y: ops.b.y, Button: tea.MouseLeft})
	if r.d.active != "wops" {
		t.Fatal("a click on Ops did not show it")
	}
	r.press("super+1")
	r.view()
	var pane hit
	for _, h := range r.d.hits {
		if h.pane && h.session == "api" {
			pane = h
		}
	}
	ft.Emit([]byte("\x1b[?1000h\x1b[?1006h")) // the program wants the mouse, as tmux's mouse mode does
	time.Sleep(50 * time.Millisecond)
	r.send(tea.MouseClickMsg{X: pane.term.x + 3, Y: pane.term.y + 2, Button: tea.MouseLeft})
	if r.d.mode != typeMode {
		t.Fatal("a click in the pane did not type into it")
	}
	r.send(tea.MouseWheelMsg{X: pane.term.x + 3, Y: pane.term.y + 2, Button: tea.MouseWheelUp})
	eventually(t, "the wheel, at the pane's cell", typedIn(ft, "\x1b[<64;4;3M"))
}

// stubClient gives the terminal manager its connections, and fails the
// rest.
type stubClient struct {
	fleetapi.Client
	mu    sync.Mutex
	dials []bool // each dial's force
	conns chan *stubConn
}

func (s *stubClient) DeskTerm(ctx context.Context, name string, cols, rows int, force bool) (fleetapi.TermConn, error) {
	s.mu.Lock()
	s.dials = append(s.dials, force)
	s.mu.Unlock()
	select {
	case c := <-s.conns:
		if c == nil {
			return nil, errors.New("dial refused")
		}
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type stubConn struct {
	frames chan *fleetapi.TermFrame
}

func (c *stubConn) Recv(ctx context.Context) ([]byte, *fleetapi.TermFrame, error) {
	select {
	case f, ok := <-c.frames:
		if !ok {
			return nil, nil, errors.New("connection lost")
		}
		return nil, f, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}
func (c *stubConn) Send(context.Context, []byte) error     { return nil }
func (c *stubConn) Resize(context.Context, int, int) error { return nil }
func (c *stubConn) Close() error                           { return nil }

// A lost connection comes back by itself, taking the keyboard it had; one
// another terminal types in waits until the operator takes it over.
func TestATerminalReconnectsAndTakesOver(t *testing.T) {
	minBackTerm = 10 * time.Millisecond
	defer func() { minBackTerm = 500 * time.Millisecond }()
	cl := &stubClient{conns: make(chan *stubConn, 4)}
	m := newTermMgr(context.Background(), cl)
	tm := m.get("api", 80, 24)
	c1 := &stubConn{frames: make(chan *fleetapi.TermFrame, 4)}
	cl.conns <- c1
	m.keep([]string{"api"}, nil, map[string]bool{"api": true}, time.Now())
	c1.frames <- &fleetapi.TermFrame{Type: fleetapi.FrameHello}
	eventually(t, "live", func() bool { st, _, _ := tm.status(); return st == termLive })
	c2 := &stubConn{frames: make(chan *fleetapi.TermFrame, 4)}
	cl.conns <- c2
	close(c1.frames) // the link drops
	c2.frames <- &fleetapi.TermFrame{Type: fleetapi.FrameHello}
	eventually(t, "live again", func() bool {
		cl.mu.Lock()
		n := len(cl.dials)
		cl.mu.Unlock()
		st, _, _ := tm.status()
		return n == 2 && st == termLive
	})
	cl.mu.Lock()
	dials := slices.Clone(cl.dials)
	cl.mu.Unlock()
	if !slices.Equal(dials, []bool{false, true}) {
		t.Fatalf("dials (force) %v: the reconnect must take back the keyboard it had", dials)
	}
	// Another terminal took it: this one watches, until taken back.
	c2.frames <- &fleetapi.TermFrame{Type: fleetapi.FrameReleased, Holder: "op (laptop)"}
	eventually(t, "released", func() bool { st, h, _ := tm.status(); return st == termReleased && h == "op (laptop)" })
	c3 := &stubConn{frames: make(chan *fleetapi.TermFrame, 4)}
	cl.conns <- c3
	m.takeOver(tm)
	c3.frames <- &fleetapi.TermFrame{Type: fleetapi.FrameHello}
	eventually(t, "taken back", func() bool { st, _, _ := tm.status(); return st == termLive })
}

// Hidden terminals stay connected a while, the most recent first; the
// rest let go, their screens kept.
func TestColdTerminalsLetGo(t *testing.T) {
	keep := warmKeep
	warmKeep = 1
	defer func() { warmKeep = keep }()
	cl := &stubClient{conns: make(chan *stubConn, 8)}
	m := newTermMgr(context.Background(), cl)
	desk := map[string]bool{"a": true, "b": true, "c": true}
	now := time.Now()
	for i, n := range []string{"a", "b", "c"} {
		m.get(n, 80, 24)
		c := &stubConn{frames: make(chan *fleetapi.TermFrame, 2)}
		c.frames <- &fleetapi.TermFrame{Type: fleetapi.FrameHello}
		cl.conns <- c
		m.keep([]string{n}, nil, desk, now.Add(time.Duration(i)*time.Second))
		eventually(t, n+" live", func() bool { st, _, _ := m.peek(n).status(); return st == termLive })
	}
	// c is shown; b was shown last before it, so b stays and a goes.
	if st, _, _ := m.peek("a").status(); st != termIdle {
		t.Fatalf("a: %v, want let go", st)
	}
	if st, _, _ := m.peek("b").status(); st != termLive {
		t.Fatalf("b: %v, want kept", st)
	}
	m.keep([]string{"c"}, nil, map[string]bool{"c": true}, now.Add(3*time.Second))
	if m.peek("a") != nil || m.peek("b") != nil {
		t.Fatal("sessions gone from the desk kept their terminals")
	}
}

// What Claude Code says through its hooks is news: a session off screen
// (or on screen in a window not looked at) is marked, listed, said, and
// sent to the desktop; u goes to it, ! lists them all.
func TestClaudeCodesNotesAreNews(t *testing.T) {
	r := newRig(t, 160, 40)
	at := tabsNow.Add(time.Minute)
	r.f.SetDeskNote("notes", fleetapi.NoteNeeds, "needs your permission to use Bash", at)
	var raw []string
	for _, m := range runCmd(r.d.Update(FrameMsg{}, r.c)) { // the poll's answer
		for _, out := range runCmd(r.d.Update(m, r.c)) {
			if rm, ok := out.(tea.RawMsg); ok {
				raw = append(raw, fmt.Sprint(rm.Msg))
			}
		}
	}
	if r.d.unread["notes"] != fleetapi.NoteNeeds || len(r.d.notices) != 1 || r.d.notices[0].text != "needs your permission to use Bash" {
		t.Fatalf("unread %v, notices %+v", r.d.unread, r.d.notices)
	}
	if !slices.ContainsFunc(raw, func(s string) bool {
		return strings.Contains(s, "777;notify;Claude · notes;needs your permission to use Bash")
	}) {
		t.Fatalf("no desktop notice in %q", raw)
	}
	v := r.view()
	if !strings.Contains(v, "◆ 2 need you") || !strings.Contains(v, "notes needs your permission") {
		t.Fatalf("the desk does not say it:\n%s", v)
	}
	if r.d.WindowTitle() != "◆ 2 need you · fleetctl" {
		t.Fatalf("title %q", r.d.WindowTitle())
	}
	r.press("!")
	if v := r.view(); !strings.Contains(v, "From Claude") || !strings.Contains(v, "needs your permission to use Bash") {
		t.Fatalf("the notices:\n%s", v)
	}
	r.press("esc", "u")
	if r.d.active != "wops" || r.d.focused() != "notes" || r.d.mode != typeMode || r.d.unread["notes"] != "" {
		t.Fatalf("u went to %s/%s mode %v", r.d.active, r.d.focused(), r.d.mode)
	}
	// On screen, in a window looked at: no news. In a window not looked at: news.
	r.f.SetDeskNote("notes", fleetapi.NoteDone, "finished its turn", at.Add(time.Minute))
	r.send(FrameMsg{})
	if len(r.d.notices) != 1 {
		t.Fatalf("a session on screen made news: %+v", r.d.notices)
	}
	r.send(tea.BlurMsg{})
	r.f.SetDeskNote("notes", fleetapi.NoteNeeds, "has a question for you", at.Add(2*time.Minute))
	r.send(FrameMsg{})
	if len(r.d.notices) != 2 || r.d.notices[0].text != "has a question for you" {
		t.Fatalf("a window not looked at: %+v", r.d.notices)
	}
}

// Too many tabs for the bar: a window of them around the focused one, and
// how many more each side; never a cut tab.
func TestTheTabBarKeepsTheFocusedTabInView(t *testing.T) {
	r := newRig(t, 100, 30)
	for i := range 9 {
		s, err := r.f.DeskCreate(context.Background(), fleetapi.DeskCreate{Dir: "~/code/api", Name: fmt.Sprintf("long-session-name-%d", i), Workspace: "main"})
		if err != nil {
			t.Fatal(err)
		}
		r.f.DeskUpdate(context.Background(), s.Name, fleetapi.DeskUpdate{Label: ptr(fmt.Sprintf("A rather long session title number %d", i))})
	}
	r.send(FrameMsg{})
	r.d.focus["main"] = "long-session-name-8"
	r.send(FrameMsg{})
	bar := strings.Split(r.view(), "\n")[1]
	if !strings.Contains(bar, "‹") || !strings.Contains(bar, "number 8") {
		t.Fatalf("the bar: %q", bar)
	}
}

func ptr[T any](v T) *T { return &v }

// A question is shown whole, wrapped; hints that do not fit are left out
// whole.
func TestQuestionsWrapAndHintsDropWhole(t *testing.T) {
	th := NewTheme(true)
	cf := &confirm{ask: "End A rather long session title that goes on and on? Its Claude Code session stays on disk."}
	card := ansi.Strip(strings.Join(confirmCard(th, cf, 40), "\n"))
	for _, w := range strings.Fields(cf.ask) {
		if !strings.Contains(card, w) {
			t.Errorf("the card lost %q:\n%s", w, card)
		}
	}
	if strings.Contains(card, "…") {
		t.Errorf("the card cut its question:\n%s", card)
	}
	got := ansi.Strip(hintsFit(th, 20, "⏎ type", "n new session", "x end"))
	if got != "⏎ type · x end" {
		t.Errorf("hintsFit = %q", got)
	}
	if l := ansi.Strip(line(10, nil, seg{s: "abcdefghijklmnop"})); l != "abcdefghi…" {
		t.Errorf("line = %q", l)
	}
}

// Each other workspace's focused session is connected before it is shown:
// the first visit to a workspace is as quick as the next.
func TestOtherWorkspacesAreReadyBeforeShown(t *testing.T) {
	r := newRig(t, 140, 40)
	eventually(t, "Ops's focused session connected unseen", func() bool {
		if r.f.DeskTermOf("infra") == nil {
			return false
		}
		st, _, _ := r.d.term("infra").status()
		return st == termLive
	})
	if r.f.DeskTermOf("notes") != nil {
		t.Fatal("a session neither shown nor focused was connected")
	}
}
