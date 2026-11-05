package tui

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/hostfiles"
)

// filesRig is the Files tab on a fake fleetd whose machine's files are a
// test's own home; here is this machine's folder for the other side.
type filesRig struct {
	t      *testing.T
	tab    *filesTab
	c      *Ctx
	f      *fleetapi.Fake
	home   string // fleetd's
	here   string // this machine's
	toasts []string
	raw    []string
}

func newFilesRig(t *testing.T) *filesRig {
	t.Helper()
	oldRest, oldTick := restFor, xferTick
	restFor, xferTick = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { restFor, xferTick = oldRest, oldTick })
	root := t.TempDir()
	home, here := filepath.Join(root, "kali", "home"), filepath.Join(root, "mac")
	for p, s := range map[string]string{
		"code/app/main.go": "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n",
		"notes.md":         "# Notes\n\nSome **bold** words.\n",
		"data.bin":         "\x00\x01\x02binary\x00",
		".hidden":          "secret-ish",
		"file2.txt":        "two",
		"file10.txt":       "ten",
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(home, p)), 0o755)
		os.WriteFile(filepath.Join(home, p), []byte(s), 0o644)
	}
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for x := range 64 {
		for y := range 32 {
			img.Set(x, y, color.RGBA{uint8(x * 4), 40, uint8(y * 8), 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	os.WriteFile(filepath.Join(home, "pic.png"), b.Bytes(), 0o644)
	os.MkdirAll(filepath.Join(here, "Downloads"), 0o755)
	os.WriteFile(filepath.Join(here, "report.txt"), []byte("from the mac"), 0o644)

	f := fleetapi.NewFake(fleetapi.RoleAdmin, func() time.Time { return tabsNow })
	f.HostFiles = hostfiles.Direct(&hostfiles.Manager{Home: home, State: filepath.Join(root, "kali", "state"), Host: "kali"})
	tab := newFiles().(*filesTab)
	tab.gfx, tab.asked, tab.cw, tab.ch = gfxBlocks, true, 8, 16
	tab.hereHome, tab.here = here, &hostfiles.Manager{Home: here}
	tab.panes[sideHere].path = here
	tab.downDir = filepath.Join(here, "Downloads")
	r := &filesRig{t: t, tab: tab, f: f, home: home, here: here,
		c: &Ctx{Client: f, Theme: NewTheme(true), Now: tabsNow, NoAnim: true}}
	r.send(tea.WindowSizeMsg{Width: 124, Height: 40})
	return r
}

// send gives the tab a message and runs what follows from it, until it
// settles.
func (r *filesRig) send(msg tea.Msg) {
	r.t.Helper()
	r.run(r.tab.Update(msg, r.c))
}

func (r *filesRig) press(keys ...string) {
	r.t.Helper()
	for _, k := range keys {
		r.send(keyMsg(k))
	}
}

func (r *filesRig) typeText(s string) {
	for _, ch := range s {
		r.send(tea.KeyPressMsg{Code: ch, Text: string(ch)})
	}
}

func (r *filesRig) run(cmd tea.Cmd) {
	r.t.Helper()
	queue := []tea.Cmd{cmd}
	for n := 0; len(queue) > 0 && n < 400; n++ {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		ch := make(chan tea.Msg, 1)
		go func() { ch <- c() }()
		var msg tea.Msg
		select {
		case msg = <-ch:
		case <-time.After(5 * time.Second):
			r.t.Fatal("a command did not end")
		}
		switch m := msg.(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, m...)
		case ToastMsg:
			r.toasts = append(r.toasts, m.Text)
		case tea.RawMsg:
			r.raw = append(r.raw, m.Msg.(string))
		default:
			queue = append(queue, r.tab.Update(m, r.c))
		}
	}
}

func (r *filesRig) view() string {
	if r.tab.view != nil {
		return r.tab.View(r.c, 124, 40)
	}
	return r.tab.View(r.c, 120, 34)
}

func (r *filesRig) shows(want ...string) {
	r.t.Helper()
	v := ansi.Strip(r.view())
	for _, w := range want {
		if !strings.Contains(v, w) {
			r.t.Fatalf("the tab does not show %q:\n%s", w, v)
		}
	}
}

func (r *filesRig) hides(not ...string) {
	r.t.Helper()
	v := ansi.Strip(r.view())
	for _, w := range not {
		if strings.Contains(v, w) {
			r.t.Fatalf("the tab shows %q:\n%s", w, v)
		}
	}
}

func (r *filesRig) to(name string) {
	r.t.Helper()
	pn := r.tab.pane()
	for i, e := range pn.rows {
		if e.Name == name {
			pn.cur = i
			r.run(r.tab.rest())
			return
		}
	}
	r.t.Fatalf("no %s in %s", name, pn.path)
}

func (r *filesRig) toasted(part string) {
	r.t.Helper()
	for _, s := range r.toasts {
		if strings.Contains(s, part) {
			return
		}
	}
	r.t.Fatalf("no toast says %q: %q", part, r.toasts)
}

func TestFilesListsFleetdsHomeFoldersFirstAndPreviewsTheFile(t *testing.T) {
	r := newFilesRig(t)
	r.shows("kali", "~", "code/", "notes.md", "pic.png")
	r.hides(".hidden")
	pn := r.tab.pane()
	var order []string
	for _, e := range pn.rows {
		order = append(order, e.Name)
	}
	if got := strings.Join(order, " "); got != "code data.bin file2.txt file10.txt notes.md pic.png" {
		t.Fatalf("the order: %s", got)
	}
	// The folder under the cursor is listed beside it before it is opened.
	r.shows("app/")
	r.to("notes.md")
	r.shows("Notes", "Some bold words")
	r.to("data.bin")
	r.shows("00000000  00 01 02")
	r.to("pic.png")
	if v := r.view(); !strings.Contains(v, "▀") {
		t.Fatalf("the picture is not drawn:\n%s", ansi.Strip(v))
	}
	r.press(".")
	r.shows(".hidden")
}

func TestFilesGoesIntoAFolderAndBackToWhereItWas(t *testing.T) {
	r := newFilesRig(t)
	r.to("code")
	r.press("enter")
	r.shows("~/code", "app/")
	r.press("enter")
	r.shows("~/code/app", "main.go")
	r.press("left", "left")
	if e, _ := r.tab.pane().entry(); r.tab.pane().path != r.home || e.Name != "code" {
		t.Fatalf("back at %s on %s", r.tab.pane().path, e.Name)
	}
	r.press("g")
	r.typeText("/")
	r.tab.ask.in.SetValue("~/code/app")
	r.press("enter")
	r.shows("~/code/app", "main.go")
}

func TestTheViewerShowsCodeAPageAndAPicture(t *testing.T) {
	r := newFilesRig(t)
	r.to("code")
	r.press("enter", "enter") // into app
	r.to("main.go")
	r.press("enter")
	if !r.tab.Fullscreen() || !r.tab.Capturing() {
		t.Fatal("the viewer is not full screen")
	}
	r.shows("main.go", "1  package main", `4      println("hi")`, "esc back")
	r.press("esc")
	if r.tab.view != nil {
		t.Fatal("esc did not close the viewer")
	}
	r.press("left", "left")
	r.to("notes.md")
	r.press("enter")
	r.shows("Notes", "Some bold words")
	r.hides("# Notes")
	r.press("m")
	r.shows("# Notes", "**bold**")
	r.press("esc")
	r.to("pic.png")
	r.press("enter")
	if v := r.view(); !strings.Contains(v, "▀") {
		t.Fatalf("the picture is not drawn:\n%s", ansi.Strip(v))
	}
	r.press("left") // the folder's file before it
	r.shows("notes.md")
}

func TestFindInTheViewer(t *testing.T) {
	r := newFilesRig(t)
	r.to("code")
	r.press("enter", "enter")
	r.to("main.go")
	r.press("enter", "/")
	r.typeText("println")
	r.press("enter")
	if v := r.tab.view; len(v.hits) != 1 || v.hits[0] != 3 {
		t.Fatalf("hits %v", v.hits)
	}
	r.shows("1 of 1")
}

func TestADownloadLandsHereAndAnUploadThere(t *testing.T) {
	r := newFilesRig(t)
	r.to("notes.md")
	r.press("d")
	got, err := os.ReadFile(filepath.Join(r.here, "Downloads", "notes.md"))
	if err != nil || !strings.HasPrefix(string(got), "# Notes") {
		t.Fatalf("downloaded %q %v", got, err)
	}
	r.toasted("downloaded notes.md to")
	// Again: beside it.
	r.press("d")
	if _, err := os.Stat(filepath.Join(r.here, "Downloads", "notes 2.md")); err != nil {
		t.Fatal("a second download went over the first")
	}
	// A folder comes whole.
	r.to("code")
	r.press("d")
	if b, _ := os.ReadFile(filepath.Join(r.here, "Downloads", "code", "app", "main.go")); !strings.Contains(string(b), "package main") {
		t.Fatal("the folder did not come")
	}
	// A file dropped on the window goes up into the folder shown.
	r.send(tea.PasteMsg{Content: filepath.Join(r.here, "report.txt")})
	if b, _ := os.ReadFile(filepath.Join(r.home, "report.txt")); string(b) != "from the mac" {
		t.Fatalf("uploaded %q", b)
	}
	r.toasted("report.txt is on kali")
	r.shows("report.txt")
	if left, _ := filepath.Glob(filepath.Join(r.here, "Downloads", ".*fleetpart*")); len(left) > 0 {
		t.Fatalf("parts left behind: %v", left)
	}
	r.press("T")
	r.shows("Transfers", "report.txt", "done")
}

func TestBothSidesCopyAcross(t *testing.T) {
	r := newFilesRig(t)
	r.press("L")
	if !r.tab.dual || r.tab.side != sideHere {
		t.Fatal("L did not show this machine's side")
	}
	r.shows("this machine", "report.txt", "kali")
	r.to("report.txt")
	r.press("space")
	if len(r.tab.pane().marks) != 1 {
		t.Fatal("space did not mark")
	}
	r.press("c")
	if b, _ := os.ReadFile(filepath.Join(r.home, "report.txt")); string(b) != "from the mac" {
		t.Fatalf("copied across %q", b)
	}
	if !r.tab.Claims("tab") {
		t.Fatal("with both sides shown, tab is not the tab's")
	}
	r.press("tab")
	if r.tab.side != sideHost {
		t.Fatal("tab did not move to fleetd's side")
	}
	r.to("notes.md")
	r.press("c")
	if _, err := os.Stat(filepath.Join(r.here, "notes.md")); err != nil {
		t.Fatal("copied across to this machine: not there")
	}
	r.press("L")
	if r.tab.Claims("tab") {
		t.Fatal("one side shown: tab is the app's")
	}
}

func TestANewFolderARenameAndTheTrash(t *testing.T) {
	r := newFilesRig(t)
	r.press("n")
	r.typeText("made")
	r.press("enter")
	if fi, err := os.Stat(filepath.Join(r.home, "made")); err != nil || !fi.IsDir() {
		t.Fatal("no folder made")
	}
	if e, _ := r.tab.pane().entry(); e.Name != "made" {
		t.Fatalf("the cursor is on %s, not the new folder", e.Name)
	}
	r.press("r")
	r.tab.ask.in.SetValue("kept")
	r.press("enter")
	if _, err := os.Stat(filepath.Join(r.home, "kept")); err != nil {
		t.Fatal("not renamed")
	}
	r.to("kept")
	r.press("x")
	r.shows("Move kept to the trash on kali?")
	r.press("y")
	if _, err := os.Stat(filepath.Join(r.home, "kept")); !os.IsNotExist(err) {
		t.Fatal("not in the trash")
	}
	r.toasted("in the trash")
}

func TestTheFilterNarrowsTheFolder(t *testing.T) {
	r := newFilesRig(t)
	r.press("/")
	if !r.tab.Capturing() {
		t.Fatal("the filter does not take the keys")
	}
	r.typeText("file")
	r.press("enter")
	if n := len(r.tab.pane().rows); n != 2 {
		t.Fatalf("%d rows", n)
	}
	r.shows("file2.txt", "file10.txt")
	r.hides("notes.md")
	r.press("esc")
	r.shows("notes.md")
}

// A kitty terminal is sent the picture once, and shown it where its
// placeholders are.
func TestAKittyTerminalGetsThePictureAndItsPlaceholders(t *testing.T) {
	r := newFilesRig(t)
	r.tab.gfx = gfxKitty
	r.to("pic.png")
	sent := strings.Join(r.raw, "")
	if !strings.Contains(sent, "\x1b_G") || !strings.Contains(sent, "U=1") {
		t.Fatalf("the picture was not sent: %q", sent[:min(len(sent), 200)])
	}
	if v := r.view(); !strings.ContainsRune(v, kitty.Placeholder) {
		t.Fatalf("no placeholders:\n%s", ansi.Strip(v))
	}
	n := len(r.raw)
	r.send(FrameMsg{})
	r.send(FrameMsg{})
	if len(r.raw) != n {
		t.Fatal("the picture was sent again")
	}
}

func TestATerminalThatAnswersKittysQueryGetsPictures(t *testing.T) {
	r := newFilesRig(t)
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("KONSOLE_VERSION", "")
	t.Setenv("TMUX", "")
	r.send(uvKitty(kittyQueryID, "OK"))
	if r.tab.gfx != gfxKitty {
		t.Fatal("an OK did not turn pictures on")
	}
}

func TestAgesTakeOneUnit(t *testing.T) {
	for d, want := range map[time.Duration]string{30 * time.Second: "30s", 5 * time.Minute: "5m", 30 * time.Hour: "30h",
		(28*24 + 15) * time.Hour: "28d", 800 * 24 * time.Hour: "2y"} {
		if got := ageShort(d); got != want {
			t.Errorf("%s: %s, want %s", d, got, want)
		}
	}
}

func TestNaturalOrder(t *testing.T) {
	for _, c := range [][2]string{{"file2", "file10"}, {"a", "B"}, {"img9.png", "img10.png"}, {"x01", "x2"}} {
		if natural(c[0], c[1]) >= 0 {
			t.Errorf("%s does not come before %s", c[0], c[1])
		}
	}
}

func uvKitty(id int, payload string) tea.Msg {
	var e uv.KittyGraphicsEvent
	e.Options.ID = id
	e.Payload = []byte(payload)
	return e
}
