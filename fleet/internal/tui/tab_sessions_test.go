package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Tests of the Sessions tab, on the Fake's demo desk: api (opus, at work,
// one terminal attached), web (restored 3 minutes ago), infra (a shell) and
// notes (sonnet), on devbox.

// deskSpy is the Fake, recording the desk calls the tab makes.
type deskSpy struct {
	*fleetapi.Fake
	lines []int    // each Desk call's lines
	dirs  []string // each DeskDirs call's prefix
	fail  error    // Desk fails with it when set
}

func (d *deskSpy) Desk(ctx context.Context, lines int) (*fleetapi.DeskList, error) {
	d.lines = append(d.lines, lines)
	if d.fail != nil {
		return nil, d.fail
	}
	return d.Fake.Desk(ctx, lines)
}

func (d *deskSpy) DeskDirs(ctx context.Context, prefix string) ([]string, error) {
	d.dirs = append(d.dirs, prefix)
	return d.Fake.DeskDirs(ctx, prefix)
}

// deskCtx is an admin's Ctx on the demo desk; attaches go into the list
// it returns, as "worker mode".
func deskCtx(t *testing.T) (*Ctx, *deskSpy, *[]string) {
	t.Helper()
	c, f := tabCtx(t, fleetapi.RoleAdmin)
	spy := &deskSpy{Fake: f}
	c.Client = spy
	got := &[]string{}
	c.Attach = func(w string, m fleetapi.TermMode) tea.Cmd {
		*got = append(*got, w+" "+string(m))
		return nil
	}
	return c, spy, got
}

// deskOpen is the tab drawn at w×h with the desk's first answer in.
func deskOpen(t *testing.T, w, h int) (*Ctx, *deskSpy, *[]string, *deskTab) {
	t.Helper()
	c, spy, got := deskCtx(t)
	tab := newSessions().(*deskTab)
	tabSettle(tab, c, w, h)
	if len(spy.lines) != 1 {
		t.Fatalf("drawing the tab polled the desk %d times, want once", len(spy.lines))
	}
	return c, spy, got, tab
}

// deskKey is tabKey with the keys the launch form takes.
func deskKey(k string) tea.KeyPressMsg {
	switch k {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "ctrl+a":
		return tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl}
	}
	if d, ok := strings.CutPrefix(k, "alt+"); ok {
		return tea.KeyPressMsg{Code: rune(d[0]), Mod: tea.ModAlt}
	}
	return tabKey(k)
}

// deskPress presses keys and returns the last one's command.
func deskPress(tab Tab, c *Ctx, keys ...string) tea.Cmd {
	var cmd tea.Cmd
	for _, k := range keys {
		cmd = tab.Update(deskKey(k), c)
	}
	return cmd
}

// deskType types s into the tab.
func deskType(tab Tab, c *Ctx, s string) {
	for _, r := range s {
		tab.Update(tea.KeyPressMsg{Code: r, Text: string(r)}, c)
	}
}

// deskDirsSettle delivers the launch form's pause in typing, and fleetd's
// answer to the lookup it makes.
func deskDirsSettle(tab *deskTab, c *Ctx) {
	tabFeed(tab, c, tabRun(tab.Update(dirsTickMsg{n: tab.form.n}, c)))
}

func strip(tab Tab, c *Ctx, w, h int) string { return ansi.Strip(tab.View(c, w, h)) }

func TestSessionsIsForAdmins(t *testing.T) {
	tab := newSessions()
	if tab.Title() != "Sessions" {
		t.Errorf("title %q", tab.Title())
	}
	if !tab.Visible(fleetapi.Me{Role: fleetapi.RoleAdmin}) || tab.Visible(fleetapi.Me{Role: fleetapi.RoleGuest}) || tab.Visible(fleetapi.Me{}) {
		t.Error("Sessions is for admins only")
	}
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	var titles []string
	for _, tb := range a.visible() {
		titles = append(titles, tb.Title())
	}
	if got := strings.Join(titles, " "); !strings.Contains(got, "Screens Sessions") {
		t.Errorf("an admin's tabs are %s: want Sessions right after Screens", got)
	}
	g, gf := testApp(t, fleetapi.RoleGuest, 120, 40, false)
	snap(t, g, gf)
	for _, tb := range g.visible() {
		if tb.Title() == "Sessions" {
			t.Error("a guest sees the Sessions tab")
		}
	}
}

func TestSessionsShowTheDesk(t *testing.T) {
	c, spy, _, tab := deskOpen(t, 120, 40)
	v := strip(tab, c, 120, 40)
	for _, want := range []string{
		"devbox", "4 sessions", "1 working",
		"1 ● api", "~/code/api", "opus", "⠋ working", "● 1 attached",
		"2 ● web", "↻ restored 3m", "ready",
		"3 ● infra", "shell", "Plan: 1 to add",
		"4 ● notes", "sonnet", "postmortem",
		"Percolating…",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("the grid lacks %q:\n%s", want, v)
		}
	}
	// Two columns, two rows: each tile shows as many lines as it holds.
	_, _, tileH := deskGrid(4, 120, 39)
	if spy.lines[0] < tileH-2 {
		t.Errorf("asked for %d lines, a tile holds %d", spy.lines[0], tileH-2)
	}
	lines := strings.Split(v, "\n")
	if !strings.HasPrefix(lines[1], "┏") || !strings.Contains(lines[1], "api") || !strings.Contains(lines[1], "╭") {
		t.Errorf("want api selected (heavy) beside web in the first row:\n%s", v)
	}
}

func TestSessionsKeysAttachAndWatch(t *testing.T) {
	c, _, got, tab := deskOpen(t, 120, 40) // api web / infra notes
	deskPress(tab, c, "enter")
	deskPress(tab, c, "right", "w")
	deskPress(tab, c, "down", "a")
	deskPress(tab, c, "h", "w")
	deskPress(tab, c, "alt+2")
	want := []string{"desk:api type", "desk:web watch", "desk:notes type", "desk:infra watch", "desk:web type"}
	if !slices.Equal(*got, want) {
		t.Errorf("attached %v, want %v", *got, want)
	}
	if tab.sel != "web" {
		t.Errorf("alt+2 left %q selected, want web", tab.sel)
	}
	if msgs := tabRun(deskPress(tab, c, "alt+7")); len(msgs) != 1 || !msgs[0].(ToastMsg).Err {
		t.Errorf("alt+7 with four sessions: %v", msgs)
	}
	if len(*got) != len(want) {
		t.Errorf("alt+7 attached: %v", *got)
	}
}

// Back from a session (its terminal closed: the app's toast says so), the
// desk is polled at once, not 2 s later: what was done there shows now.
func TestSessionsRefreshOnDetach(t *testing.T) {
	c, spy, _, tab := deskOpen(t, 120, 40)
	tabFeed(tab, c, tabRun(tab.Update(ToastMsg{Text: "ended task"}, c)))
	if len(spy.lines) != 1 {
		t.Fatalf("a toast with no attach out polled the desk")
	}
	deskPress(tab, c, "enter")
	tabFeed(tab, c, tabRun(tab.Update(ToastMsg{Text: "back from desk:api"}, c)))
	if len(spy.lines) != 2 {
		t.Errorf("coming back from api did not poll at once: %d polls", len(spy.lines))
	}
	tabFeed(tab, c, tabRun(deskPress(tab, c, "r")))
	if len(spy.lines) != 3 {
		t.Errorf("r did not poll at once: %d polls", len(spy.lines))
	}
}

func TestSessionsEndAsksFirst(t *testing.T) {
	c, spy, _, tab := deskOpen(t, 120, 40)
	deskPress(tab, c, "x")
	if v := strip(tab, c, 120, 40); !strings.Contains(v, "End api? Its Claude Code session stays on disk.") {
		t.Errorf("x did not ask:\n%s", v)
	}
	if !tab.Capturing() {
		t.Error("the question does not hold the keys")
	}
	deskPress(tab, c, "n")
	if len(spy.Calls) != 0 || tab.Capturing() {
		t.Fatalf("n ended it: %v", spy.Calls)
	}
	deskPress(tab, c, "right", "right") // infra, a shell
	deskPress(tab, c, "x")
	if v := strip(tab, c, 120, 40); !strings.Contains(v, "End infra? What runs in it stops.") {
		t.Errorf("a shell's question:\n%s", v)
	}
	tabFeed(tab, c, tabRun(deskPress(tab, c, "y")))
	if !slices.Equal(spy.Calls, []string{"desk end infra"}) {
		t.Errorf("calls %v, want one end of infra", spy.Calls)
	}
	if v := strip(tab, c, 120, 40); strings.Contains(v, "~/code/infra") || !strings.Contains(v, "3 sessions") {
		t.Errorf("infra is still shown:\n%s", v)
	}
}

func TestSessionsLaunchWithTheForm(t *testing.T) {
	c, spy, got, tab := deskOpen(t, 120, 40)
	deskPress(tab, c, "n")
	if !tab.Capturing() {
		t.Fatal("n opened no form")
	}
	v := strip(tab, c, 120, 40)
	for _, want := range []string{"New session", "on devbox", "Directory", "recent", "/home/op/code/api", "Claude Opus", "Shell", "Custom…", "attach on launch"} {
		if !strings.Contains(v, want) {
			t.Errorf("the form lacks %q:\n%s", want, v)
		}
	}
	deskType(tab, c, "/home/op/code/w")
	if len(spy.dirs) != 0 {
		t.Fatalf("asked for directories before typing paused: %v", spy.dirs)
	}
	tabFeed(tab, c, tabRun(tab.Update(dirsTickMsg{n: tab.form.n - 1}, c)))
	if len(spy.dirs) != 0 {
		t.Fatalf("a pause after an earlier key asked: %v", spy.dirs)
	}
	deskDirsSettle(tab, c)
	if !slices.Equal(spy.dirs, []string{"/home/op/code/w"}) {
		t.Fatalf("asked %v, want what was typed once it paused", spy.dirs)
	}
	if v := strip(tab, c, 120, 40); !strings.Contains(v, "▸ /home/op/code/web") {
		t.Errorf("no completion offered:\n%s", v)
	}
	deskPress(tab, c, "down")  // past the only offer: the command
	deskPress(tab, c, "right") // Claude Opus
	deskPress(tab, c, "down")  // the name
	deskType(tab, c, "frontend")
	tabFeed(tab, c, tabRun(deskPress(tab, c, "enter")))
	want := `desk new name="frontend" dir="/home/op/code/web" command="claude --model opus"`
	if !slices.Equal(spy.Calls, []string{want}) {
		t.Fatalf("calls %v, want %s", spy.Calls, want)
	}
	if tab.Capturing() {
		t.Error("the form stayed open after the launch")
	}
	if tab.sel != "frontend" {
		t.Errorf("selected %q, want the new session", tab.sel)
	}
	if !slices.Equal(*got, []string{"desk:frontend type"}) {
		t.Errorf("attached %v, want the new session at once", *got)
	}
}

func TestSessionsTabCompletesAndDescends(t *testing.T) {
	c, spy, got, tab := deskOpen(t, 120, 40)
	deskPress(tab, c, "n")
	deskType(tab, c, "/home/op/co")
	deskDirsSettle(tab, c)
	tabFeed(tab, c, tabRun(deskPress(tab, c, "tab")))
	if v := tab.form.dir.Value(); v != "/home/op/code/" {
		t.Fatalf("tab made %q, want /home/op/code/", v)
	}
	if !slices.Equal(spy.dirs, []string{"/home/op/co", "/home/op/code/"}) {
		t.Fatalf("asked %v: descending asks for what is inside at once", spy.dirs)
	}
	v := strip(tab, c, 120, 40)
	for _, want := range []string{"/home/op/code/api", "/home/op/code/infra", "/home/op/code/notes", "/home/op/code/web"} {
		if !strings.Contains(v, want) {
			t.Errorf("the offers lack %s:\n%s", want, v)
		}
	}
	deskPress(tab, c, "down") // infra
	tabFeed(tab, c, tabRun(deskPress(tab, c, "tab")))
	if v := tab.form.dir.Value(); v != "/home/op/code/infra/" {
		t.Fatalf("tab made %q", v)
	}
	deskPress(tab, c, "ctrl+a") // no attach this time
	if v := strip(tab, c, 120, 40); !strings.Contains(v, "○ attach on launch") {
		t.Errorf("ctrl+a did not turn attaching off:\n%s", v)
	}
	msgs := tabRun(deskPress(tab, c, "enter"))
	if want := `desk new name="" dir="/home/op/code/infra" command=""`; !slices.Equal(spy.Calls, []string{want}) {
		t.Fatalf("calls %v, want %s", spy.Calls, want)
	}
	var toasts []string
	for _, m := range tabRun(tab.Update(msgs[0], c)) {
		if tm, ok := m.(ToastMsg); ok {
			toasts = append(toasts, tm.Text)
		}
	}
	if len(*got) != 0 {
		t.Errorf("attached with attach on launch off: %v", *got)
	}
	if tab.sel != "infra-2" || len(toasts) != 1 || !strings.Contains(toasts[0], "launched infra-2") {
		t.Errorf("selected %q, toasts %v", tab.sel, toasts)
	}
	deskPress(tab, c, "n")
	if !strings.Contains(strip(tab, c, 120, 40), "○ attach on launch") {
		t.Error("the next form forgot attach on launch is off")
	}
}

func TestSessionsLaunchCustomAndShell(t *testing.T) {
	for _, tc := range []struct {
		keys    []string
		typed   string
		command string
	}{
		{[]string{"right", "right", "right"}, "", "shell"},
		{[]string{"right", "right", "right", "right", "down"}, "npm run dev", "npm run dev"},
		{[]string{"right", "right"}, "", "claude --model sonnet"},
	} {
		c, spy, _, tab := deskOpen(t, 120, 40)
		deskPress(tab, c, "n")
		deskType(tab, c, "~/code/notes")
		deskPress(tab, c, "tab") // no offers yet: the command
		deskPress(tab, c, tc.keys...)
		deskType(tab, c, tc.typed)
		tabFeed(tab, c, tabRun(deskPress(tab, c, "enter")))
		want := fmt.Sprintf(`desk new name="" dir="~/code/notes" command=%q`, tc.command)
		if !slices.Equal(spy.Calls, []string{want}) {
			t.Errorf("calls %v, want %s", spy.Calls, want)
		}
	}
}

// A path pasted (a paste is no key press) goes into the field and is
// looked up like one typed.
func TestSessionsPasteADirectory(t *testing.T) {
	c, spy, _, tab := deskOpen(t, 120, 40)
	deskPress(tab, c, "n")
	tab.Update(tea.PasteMsg{Content: "/home/op/code/notes"}, c)
	if v := tab.form.dir.Value(); v != "/home/op/code/notes" {
		t.Fatalf("the field holds %q after a paste", v)
	}
	deskDirsSettle(tab, c)
	if !slices.Equal(spy.dirs, []string{"/home/op/code/notes"}) {
		t.Errorf("asked %v after a paste", spy.dirs)
	}
}

// fleetd offers what is inside a directory once it is typed whole: ⏎ then
// launches in that directory, not in the first thing inside it. A name
// half typed is finished by the offer, as tab would.
func TestSessionsEnterLaunchesWhereTyped(t *testing.T) {
	for _, tc := range []struct{ typed, dir string }{
		{"/home/op/code/api", "/home/op/code/api"},
		{"~/code", "~/code"},
		{"~/code/no", "/home/op/code/notes"},
		{"co", "/home/op/code"},
	} {
		c, spy, _, tab := deskOpen(t, 120, 40)
		deskPress(tab, c, "n")
		deskType(tab, c, tc.typed)
		deskDirsSettle(tab, c)
		if len(tab.form.sugg) == 0 {
			t.Fatalf("%s: nothing offered", tc.typed)
		}
		tabFeed(tab, c, tabRun(deskPress(tab, c, "enter")))
		want := fmt.Sprintf(`desk new name="" dir=%q command=""`, tc.dir)
		if !slices.Equal(spy.Calls, []string{want}) {
			t.Errorf("%s: calls %v, want %s", tc.typed, spy.Calls, want)
		}
	}
}

// With no session to offer a recent directory, the form asks fleetd where
// to start.
func TestSessionsFormOnAnEmptyDeskOffersHome(t *testing.T) {
	c, spy := deskEmpty(t)
	tab := newSessions().(*deskTab)
	tabSettle(tab, c, 120, 40)
	tabFeed(tab, c, tabRun(deskPress(tab, c, "n")))
	if !slices.Equal(spy.dirs, []string{""}) {
		t.Fatalf("asked %v, want fleetd's starting points", spy.dirs)
	}
	if v := strip(tab, c, 120, 40); !strings.Contains(v, "▸ /home/op ") || !strings.Contains(v, "/home/op/code") {
		t.Errorf("home and the code are not offered:\n%s", v)
	}
}

func TestSessionsLaunchErrorsStayInTheForm(t *testing.T) {
	c, spy, got, tab := deskOpen(t, 120, 40)
	deskPress(tab, c, "n", "esc")
	if tab.Capturing() || len(spy.Calls) != 0 {
		t.Fatal("esc did not drop the form")
	}
	deskPress(tab, c, "n")
	deskType(tab, c, "/nowhere")
	tabFeed(tab, c, tabRun(deskPress(tab, c, "enter")))
	if !tab.Capturing() {
		t.Fatal("a failed launch closed the form")
	}
	if v := strip(tab, c, 120, 40); !strings.Contains(v, "✕ 400: no directory /nowhere on devbox") {
		t.Errorf("the error is not in the form:\n%s", v)
	}
	if len(*got) != 0 {
		t.Errorf("attached after a failed launch: %v", *got)
	}
	deskPress(tab, c, "backspace")
	if v := strip(tab, c, 120, 40); strings.Contains(v, "no directory") {
		t.Errorf("the error outlived an edit:\n%s", v)
	}
}

func TestSessionsFailedPollKeepsTheScreens(t *testing.T) {
	c, spy, _, tab := deskOpen(t, 120, 40)
	c.Now = c.Now.Add(14 * time.Second)
	tab.Update(deskMsg{err: errors.New("fleetd at http://fleet.example:7878 did not answer in time")}, c)
	v := strip(tab, c, 120, 40)
	if n := strings.Count(v, "⚠ 14s old"); n != 4 {
		t.Errorf("a failed poll: %d of 4 tiles marked with their age:\n%s", n, v)
	}
	for _, want := range []string{"Percolating", "Plan: 1 to add", "postmortem", "did not answer in time"} {
		if !strings.Contains(v, want) {
			t.Errorf("a failed poll lost %q:\n%s", want, v)
		}
	}

	// The next answers, but web's own capture failed: it keeps its screen.
	c.Now = c.Now.Add(2 * time.Second)
	l, _ := spy.Fake.Desk(context.Background(), 40)
	l.Sessions[1].Text, l.Sessions[1].Error = "", "capture timed out"
	tab.Update(deskMsg{list: l}, c)
	v = strip(tab, c, 120, 40)
	if n := strings.Count(v, "⚠"); n != 1 || !strings.Contains(v, "web ⚠ 16s old") {
		t.Errorf("want web alone marked 16s old:\n%s", v)
	}
	if !strings.Contains(v, "Shall I do that next?") || strings.Contains(v, "capture timed out") {
		t.Errorf("web's screen gave way to its error:\n%s", v)
	}

	fresh := newSessions()
	fresh.View(c, 120, 40)
	fresh.Update(deskMsg{err: errors.New("404 page not found")}, c)
	if v := strip(fresh, c, 120, 40); !strings.Contains(v, "404 page not found") {
		t.Errorf("a desk never read does not say why:\n%s", v)
	}
}

func TestSessionsZoom(t *testing.T) {
	c, spy, _, tab := deskOpen(t, 120, 40)
	deskPress(tab, c, "right")
	tabFeed(tab, c, tabRun(deskPress(tab, c, "z")))
	if got := spy.lines[len(spy.lines)-1]; got < 38 {
		t.Errorf("zoomed, asked for %d lines, want the whole height", got)
	}
	v := strip(tab, c, 120, 40)
	lines := strings.Split(v, "\n")
	if !strings.HasPrefix(lines[0], "┏") || !strings.Contains(lines[0], "web") || !strings.HasSuffix(lines[39], "┛") {
		t.Errorf("zoom is not web over the body:\n%s", v)
	}
	if !strings.Contains(v, "Welcome to Claude Code") || strings.Contains(v, "notes") {
		t.Errorf("zoom shows the wrong screen:\n%s", v)
	}
	deskPress(tab, c, "right") // moving while zoomed moves the zoom
	if v := strip(tab, c, 120, 40); !strings.Contains(strings.Split(v, "\n")[0], "infra") {
		t.Errorf("right while zoomed:\n%s", v)
	}
	deskPress(tab, c, "esc")
	if v := strip(tab, c, 120, 40); !strings.Contains(v, "notes") || !strings.Contains(v, "api") {
		t.Errorf("esc did not go back to the grid:\n%s", v)
	}
}

func TestSessionsEmptyDesk(t *testing.T) {
	c, spy := deskEmpty(t)
	tab := newSessions()
	tabSettle(tab, c, 120, 40)
	v := strip(tab, c, 120, 40)
	for _, want := range []string{"No sessions on devbox yet", "press n to start one", "restart"} {
		if !strings.Contains(v, want) {
			t.Errorf("the empty desk lacks %q:\n%s", want, v)
		}
	}
	for _, k := range []string{"enter", "x", "w", "z", "alt+1", "down"} {
		if msgs := tabRun(deskPress(tab, c, k)); len(msgs) > 0 && k != "alt+1" {
			t.Errorf("%s on an empty desk: %v", k, msgs)
		}
	}
	deskPress(tab, c, "n")
	if !tab.Capturing() {
		t.Error("n on an empty desk opened no form")
	}
	_ = spy
}

// deskEmpty is deskCtx with every session ended.
func deskEmpty(t *testing.T) (*Ctx, *deskSpy) {
	t.Helper()
	c, spy, _ := deskCtx(t)
	for _, n := range []string{"api", "web", "infra", "notes"} {
		if err := spy.Fake.DeskEnd(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	spy.Calls = nil
	return c, spy
}

// The desk is polled only while the tab is shown: hidden, its ticks reach
// another tab and stop.
func TestSessionsPollOnlyWhileShown(t *testing.T) {
	now := testNow
	clk := func() time.Time { return now }
	spy := &deskSpy{Fake: fleetapi.NewFake(fleetapi.RoleAdmin, clk)}
	a := newApp(context.Background(), Options{Client: spy, NoAnim: true, Now: clk})
	a.Init()
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	s, _ := spy.Watch(context.Background(), 0)
	a.Update(SnapshotMsg{Snap: s})
	do(a, goTab(t, a, "Sessions"))
	if len(spy.lines) != 1 {
		t.Fatalf("showing Sessions polled %d times, want once", len(spy.lines))
	}
	goTab(t, a, "Fleet")
	now = now.Add(5 * time.Second)
	do(a, func() tea.Msg { return deskTickMsg{} })
	a.Update(FrameMsg{})
	if len(spy.lines) != 1 {
		t.Errorf("polled %d times while hidden", len(spy.lines))
	}
	do(a, goTab(t, a, "Sessions"))
	if len(spy.lines) != 2 {
		t.Errorf("shown again, %d polls, want 2", len(spy.lines))
	}
}

// goTab shows the tab with this title by its number key, and returns the
// command the app returned.
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

func TestSessionsKeepTheirSize(t *testing.T) {
	sizes := [][2]int{{0, 0}, {1, 1}, {20, 5}, {40, 12}, {61, 9}, {80, 24}, {120, 40}, {200, 60}}
	for _, sz := range sizes {
		w, h := sz[0], sz[1]
		c, spy, _ := deskCtx(t)
		tab := newSessions().(*deskTab)
		tabSize(t, "before the desk", tab.View(c, w, h), w, h)
		tabSettle(tab, c, w, h)
		tabSize(t, "grid", tab.View(c, w, h), w, h)
		deskPress(tab, c, "x")
		tabSize(t, "question", tab.View(c, w, h), w, h)
		deskPress(tab, c, "n", "z")
		tabSize(t, "zoom", tab.View(c, w, h), w, h)
		deskPress(tab, c, "n")
		deskType(tab, c, "/home/op/code/")
		deskDirsSettle(tab, c)
		tabSize(t, "form", tab.View(c, w, h), w, h)
		deskPress(tab, c, "down", "down", "down", "down") // past the four offers: the command
		deskPress(tab, c, "right", "right", "right", "right", "down")
		tabSize(t, "form, custom", tab.View(c, w, h), w, h)
		deskPress(tab, c, "esc", "esc")
		tab.Update(deskMsg{err: errors.New("fleetd did not answer")}, c)
		tabSize(t, "stale", tab.View(c, w, h), w, h)
		ec, _ := deskEmpty(t)
		empty := newSessions()
		tabSettle(empty, ec, w, h)
		tabSize(t, "empty", empty.View(ec, w, h), w, h)
		_ = spy
	}
}

// wideDesk is the demo desk with wide runes everywhere.
type wideDesk struct{ *fleetapi.Fake }

func (w wideDesk) Desk(ctx context.Context, lines int) (*fleetapi.DeskList, error) {
	l, err := w.Fake.Desk(ctx, lines)
	if err != nil {
		return nil, err
	}
	for i := range l.Sessions {
		l.Sessions[i].Name = "修复登录🚀-" + l.Sessions[i].Name
		l.Sessions[i].Dir = "/home/op/code/ｗｉｄｅ/" + strings.Repeat("界", 30)
		l.Sessions[i].Text = "\x1b[1m修复登录页面🚀🚀 and a very long line that runs well past the edge of any tile at all\x1b[0m\n\ttabbed\tout\x07\n" + strings.Repeat("界", 80)
	}
	return l, nil
}

func TestSessionsKeepTheirWidthWithWideRunes(t *testing.T) {
	for _, sz := range [][2]int{{120, 40}, {61, 9}, {59, 20}, {200, 60}, {80, 24}} {
		c, f := tabCtx(t, fleetapi.RoleAdmin)
		c.Client = wideDesk{f}
		tab := newSessions()
		tabSettle(tab, c, sz[0], sz[1])
		tabSize(t, "grid", tab.View(c, sz[0], sz[1]), sz[0], sz[1])
		deskPress(tab, c, "z")
		tabSize(t, "zoom", tab.View(c, sz[0], sz[1]), sz[0], sz[1])
	}
}

func TestGoldenSessionsViews(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}, {200, 60}} {
		c, _, _, tab := deskOpen(t, sz[0], sz[1])
		view := tab.View(c, sz[0], sz[1])
		tabSize(t, "Sessions", view, sz[0], sz[1])
		tabGolden(t, fmt.Sprintf("sessions_admin_%dx%d.golden", sz[0], sz[1]), ansi.Strip(view)+"\n")
	}
	c, _, _, tab := deskOpen(t, 120, 40)
	deskPress(tab, c, "n")
	deskType(tab, c, "/home/op/code/")
	deskDirsSettle(tab, c)
	tabGolden(t, "sessions_launch_120x40.golden", strip(tab, c, 120, 40)+"\n")
	ec, _ := deskEmpty(t)
	empty := newSessions()
	tabSettle(empty, ec, 120, 40)
	tabGolden(t, "sessions_empty_120x40.golden", strip(empty, ec, 120, 40)+"\n")
}
