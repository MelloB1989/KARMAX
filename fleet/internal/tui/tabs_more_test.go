package tui

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Tests of the Screens, Quota, Hosts and Access tabs, called directly with
// a Ctx on the Fake, as the app would.

func init() {
	// app_test.go declares -update for its golden files; this file uses the
	// same flag, and declares it only when it is alone.
	if flag.Lookup("update") == nil {
		flag.Bool("update", false, "rewrite the golden files")
	}
}

func rewriteGolden() bool {
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}

var tabsNow = time.Date(2026, 10, 25, 21, 4, 0, 0, time.UTC)

func tabCtx(t *testing.T, role fleetapi.Role) (*Ctx, *fleetapi.Fake) {
	t.Helper()
	f := fleetapi.NewFake(role, func() time.Time { return tabsNow })
	snap, err := f.Watch(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return &Ctx{Snap: snap, Client: f, Theme: NewTheme(true), Now: tabsNow, NoAnim: true}, f
}

func tabKey(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	}
	r := []rune(k)[0]
	return tea.KeyPressMsg{Code: r, Text: k}
}

// tabRun runs cmd and every command it batches, and returns their messages.
// Only for commands that hold no ticks.
func tabRun(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range b {
			out = append(out, tabRun(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// tabFeed delivers msgs to tab, as the app would, and drops the commands they
// return (a screens answer returns its 2 s tick).
func tabFeed(tab Tab, c *Ctx, msgs []tea.Msg) {
	for _, m := range msgs {
		tab.Update(m, c)
	}
}

// tabSettle draws tab at w×h, lets it fetch what it shows, and feeds the
// answer back.
func tabSettle(tab Tab, c *Ctx, w, h int) {
	tab.View(c, w, h)
	tabFeed(tab, c, tabRun(tab.Update(FrameMsg{}, c)))
}

// tabSize fails unless view is exactly w×h cells.
func tabSize(t *testing.T, what, view string, w, h int) {
	t.Helper()
	if h <= 0 {
		if view != "" {
			t.Errorf("%s at %d×%d: want nothing, got %q", what, w, h, view)
		}
		return
	}
	lines := strings.Split(view, "\n")
	if len(lines) != h {
		t.Errorf("%s at %d×%d has %d lines", what, w, h, len(lines))
	}
	for i, l := range lines {
		if got := lipgloss.Width(l); got != w {
			t.Errorf("%s at %d×%d: line %d is %d cells: %q", what, w, h, i, got, ansi.Strip(l))
			return
		}
	}
}

func TestScreensFetchOnlyVisibleTiles(t *testing.T) {
	c, f := tabCtx(t, fleetapi.RoleAdmin)
	tab := newScreens()
	tab.View(c, 120, 20) // 2 columns × 2 rows
	tabFeed(tab, c, tabRun(tab.Update(FrameMsg{}, c)))
	want := []string{"agent-01", "agent-01:2", "agent-02", "agent-02:2"}
	if len(f.ScreensAsked) != 1 || !slices.Equal(f.ScreensAsked[0], want) {
		t.Fatalf("asked %v, want only %v", f.ScreensAsked, want)
	}
	if cmd := tab.Update(FrameMsg{}, c); cmd != nil {
		t.Errorf("fetched again before 2 s were up")
	}
	tab.Update(tabKey("down"), c)
	tabFeed(tab, c, tabRun(tab.Update(tabKey("down"), c))) // the third row: the grid scrolls one row
	want = []string{"agent-02", "agent-02:2", "agent-03", "agent-03:2"}
	if got := f.ScreensAsked[len(f.ScreensAsked)-1]; !slices.Equal(got, want) {
		t.Errorf("after scrolling asked %v, want %v", got, want)
	}
	if c.Selected != "agent-03" {
		t.Errorf("selected %q, want agent-03 shared with the other tabs", c.Selected)
	}
	c.Now = c.Now.Add(2 * time.Second)
	tabFeed(tab, c, tabRun(tab.Update(screensTickMsg{}, c)))
	if n := len(f.ScreensAsked); n != 3 {
		t.Errorf("%d captures after the 2 s tick, want 3", n)
	}
}

// A poll that fails, or one tile's capture that fails, never blanks a screen
// already shown: on a loaded host one slow answer is common, and a grid of
// errors over screens that were fine says the fleet is down when it is not.
// The screen stays, marked with its age; a tile with nothing yet shows why.
func TestAFailedPollKeepsTheScreensItHad(t *testing.T) {
	c, _ := tabCtx(t, fleetapi.RoleAdmin)
	tab := newScreens()
	tabSettle(tab, c, 120, 20) // agent-01, agent-01:2, agent-02, agent-02:2
	c.Now = c.Now.Add(14 * time.Second)
	tab.Update(screensMsg{err: errors.New("fleetd at http://fleet.example:7878 did not answer in time")}, c)
	v := ansi.Strip(tab.View(c, 120, 20))
	if n := strings.Count(v, "working on its task"); n != 4 {
		t.Errorf("a failed poll: %d of 4 screens still shown:\n%s", n, v)
	}
	if n := strings.Count(v, "⚠ 14s old"); n != 4 {
		t.Errorf("a failed poll: %d of 4 tiles marked with their age:\n%s", n, v)
	}
	if strings.Contains(v, "did not answer") {
		t.Errorf("an error hides a screen:\n%s", v)
	}

	// The next poll answers, but agent-02's capture failed: it keeps its old
	// screen and age, the others are fresh again.
	c.Now = c.Now.Add(2 * time.Second)
	var got []fleetapi.Screen
	for _, w := range []string{"agent-01", "agent-01:2", "agent-02", "agent-02:2"} {
		sc := fleetapi.Screen{Worker: w, Text: "> " + w + ": working on its task"}
		if w == "agent-02" {
			sc = fleetapi.Screen{Worker: w, Error: "not captured yet: the host is slow to answer"}
		}
		got = append(got, sc)
	}
	tab.Update(screensMsg{got: got}, c)
	v = ansi.Strip(tab.View(c, 120, 20))
	if n := strings.Count(v, "working on its task"); n != 4 || strings.Contains(v, "not captured") {
		t.Errorf("a failed tile: %d of 4 screens shown:\n%s", n, v)
	}
	if n := strings.Count(v, "⚠ 16s old"); n != 1 || !strings.Contains(v, "agent-02 standby ⚠ 16s old") {
		t.Errorf("a failed tile: want agent-02 alone marked 16s old:\n%s", v)
	}

	fresh := newScreens()
	fresh.View(c, 120, 20)
	fresh.Update(screensMsg{err: errors.New("fleetd at http://fleet.example:7878 did not answer in time")}, c)
	if v := ansi.Strip(fresh.View(c, 120, 20)); strings.Count(v, "did not answer in time") != 4 {
		t.Errorf("tiles never captured do not say why:\n%s", v)
	}
}

func TestZoomFillsTheBody(t *testing.T) {
	c, f := tabCtx(t, fleetapi.RoleAdmin)
	tab := newScreens()
	tabSettle(tab, c, 120, 30)
	tab.Update(tabKey("right"), c)
	tabFeed(tab, c, tabRun(tab.Update(tabKey("enter"), c)))
	if got := f.ScreensAsked[len(f.ScreensAsked)-1]; !slices.Equal(got, []string{"agent-01:2"}) {
		t.Errorf("zoomed capture asked %v, want only agent-01:2", got)
	}
	view := tab.View(c, 120, 30)
	tabSize(t, "zoom", view, 120, 30)
	lines := strings.Split(ansi.Strip(view), "\n")
	if !strings.HasPrefix(lines[0], "┏") || !strings.HasSuffix(lines[29], "┛") || !strings.Contains(lines[0], "agent-01:2") {
		t.Errorf("zoom is not one tile over the body:\n%s", ansi.Strip(view))
	}
	if !strings.Contains(ansi.Strip(view), "Welcome to Claude Code") {
		t.Errorf("zoom does not show the screen")
	}
	tab.Update(tabKey("esc"), c)
	if strings.Count(ansi.Strip(tab.View(c, 120, 30)), "Welcome") < 2 {
		t.Errorf("esc did not go back to the grid")
	}
}

func TestScreensSshIntoTheSelectedTile(t *testing.T) {
	c, _ := tabCtx(t, fleetapi.RoleAdmin)
	var got []string
	c.Attach = func(w string, m fleetapi.TermMode) tea.Cmd {
		got = append(got, w+" "+string(m))
		return nil
	}
	tab := newScreens()
	tabSettle(tab, c, 120, 30)
	tab.Update(tabKey("down"), c)
	tab.Update(tabKey("s"), c)
	tab.Update(tabKey("t"), c)
	if want := []string{"agent-02 watch", "agent-02 type"}; !slices.Equal(got, want) {
		t.Errorf("attached %v, want %v", got, want)
	}
}

func TestScreensFollowTheWorkerSelectedElsewhere(t *testing.T) {
	c, f := tabCtx(t, fleetapi.RoleAdmin)
	tab := newScreens()
	tabSettle(tab, c, 120, 20)
	c.Selected = "agent-05:2" // the Fleet tab's selection
	tabFeed(tab, c, tabRun(tab.Update(FrameMsg{}, c)))
	if got := f.ScreensAsked[len(f.ScreensAsked)-1]; !slices.Contains(got, "agent-05:2") {
		t.Errorf("asked %v, want the grid around agent-05:2", got)
	}
}

func TestQuotaShowsRunOutOnlyBeforeReset(t *testing.T) {
	c, _ := tabCtx(t, fleetapi.RoleAdmin)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 25, h, m, 0, 0, time.UTC) }
	c.Snap.Quota = []fleetapi.Account{
		{Name: "karmax", Orchestrator: true, FiveHour: 2},
		{Name: "acct-soon", Agents: []string{"agent-02"}, FiveHour: 80, BurnPerHour: 12, FiveResets: at(22, 30), RunOut: at(22, 40)},
		{Name: "acct-short", Agents: []string{"agent-04"}, FiveHour: 60, BurnPerHour: 40, FiveResets: at(23, 0), RunOut: at(22, 4)},
		{Name: "acct-calm", Agents: []string{"agent-06"}, FiveHour: 8, FiveResets: at(23, 0)},
	}
	view := ansi.Strip(newQuota().View(c, 120, 20))
	row := func(name string) string {
		for _, l := range strings.Split(view, "\n") {
			if strings.HasPrefix(l, name+" ") {
				return l
			}
		}
		t.Fatalf("no row for %s in\n%s", name, view)
		return ""
	}
	if r := row("acct-soon"); strings.Contains(r, "runs out") || !strings.Contains(r, "ok till reset") {
		t.Errorf("a run-out after the reset is shown: %q", r)
	}
	if r := row("acct-short"); !strings.Contains(r, "runs out 22:04") {
		t.Errorf("a run-out before the reset is missing: %q", r)
	}
	if r := row("acct-calm"); !strings.Contains(r, "ok till reset") {
		t.Errorf("no burn should be ok till reset: %q", r)
	}
	if strings.Index(view, "karmax") < strings.Index(view, "acct-calm") {
		t.Errorf("the orchestrator's row is not last:\n%s", view)
	}
	if !strings.Contains(view, "$41.37") || !strings.Contains(view, "1.28M") {
		t.Errorf("Today is missing:\n%s", view)
	}
}

func TestHostsMarkStaleWorkers(t *testing.T) {
	c, _ := tabCtx(t, fleetapi.RoleAdmin)
	for i := range c.Snap.Workers {
		switch c.Snap.Workers[i].Agent {
		case "agent-02:2":
			c.Snap.Workers[i].RecordedAt = tabsNow.Add(-5 * time.Minute)
		case "agent-12":
			c.Snap.Workers[i].Unreachable = true
		}
	}
	view := ansi.Strip(newHosts().View(c, 160, 60))
	var a02, a01, a12 string
	for _, l := range strings.Split(view, "\n") {
		switch {
		case strings.HasPrefix(l, "  agent-02 "):
			a02 = l
		case strings.HasPrefix(l, "  agent-01 "):
			a01 = l
		case strings.HasPrefix(l, "  agent-12 "):
			a12 = l
		}
	}
	if !strings.Contains(a02, "stale agent-02:2 5m") {
		t.Errorf("agent-02:2 is not marked stale: %q", a02)
	}
	if strings.Contains(a01, "stale") {
		t.Errorf("a fresh container is marked stale: %q", a01)
	}
	if !strings.Contains(a12, "unreachable agent-12") {
		t.Errorf("agent-12 is not marked unreachable: %q", a12)
	}
	if !strings.Contains(view, "fleetd v0.3.0-demo") || !strings.Contains(view, "tick 2140ms") {
		t.Errorf("no fleetd line:\n%s", view)
	}
	if newHosts().Visible(c.Snap.Me) == false || newHosts().Visible(fleetapi.Me{Role: fleetapi.RoleGuest}) {
		t.Errorf("Hosts is for admins only")
	}
}

func tabOpenAccess(t *testing.T) (*Ctx, *fleetapi.Fake, Tab) {
	t.Helper()
	c, f := tabCtx(t, fleetapi.RoleAdmin)
	tab := newAccess()
	tabFeed(tab, c, tabRun(tab.Update(FrameMsg{}, c)))
	if !strings.Contains(ansi.Strip(tab.View(c, 120, 30)), "friend-laptop") {
		t.Fatalf("the access list did not load:\n%s", ansi.Strip(tab.View(c, 120, 30)))
	}
	return c, f, tab
}

func TestLendOffersOnlySandboxes(t *testing.T) {
	c, f, tab := tabOpenAccess(t)
	if cmd := tab.Update(tabKey("l"), c); tab.Capturing() || cmd == nil {
		t.Errorf("lend on an admin device opened a picker")
	}
	tab.Update(tabKey("down"), c) // friend-laptop, lent agent-09
	tab.Update(tabKey("l"), c)
	if got := tab.(*access).pick.choices; !slices.Equal(got, []string{"agent-10", "agent-11"}) {
		t.Errorf("lend to friend-laptop offers %v, want the sandboxes it does not have", got)
	}
	tab.Update(tabKey("esc"), c)
	tab.Update(tabKey("down"), c) // the knock from pal-desktop
	tab.Update(tabKey("l"), c)
	if got := tab.(*access).pick.choices; !slices.Equal(got, []string{"agent-09", "agent-10", "agent-11"}) {
		t.Errorf("lend to a knock offers %v, want every sandbox", got)
	}
	if !tab.Capturing() {
		t.Errorf("the picker does not capture keys")
	}
	tab.Update(tabKey("down"), c)
	msgs := tabRun(tab.Update(tabKey("enter"), c))
	if want := "access lend nST3 agent-10"; !slices.Contains(f.Calls, want) {
		t.Fatalf("calls %v, want %q", f.Calls, want)
	}
	toasts := tabRun(tab.Update(msgs[0], c))
	view := ansi.Strip(tab.View(c, 160, 30))
	if !strings.Contains(view, "can read its Claude login") {
		t.Errorf("no caveat after a lend:\n%s", view)
	}
	if !strings.Contains(view, "pal-desktop.tail9abc.ts.net") || !strings.Contains(view, "Knocks  none") {
		t.Errorf("the lent knock is not a device now:\n%s", view)
	}
	if len(toasts) == 0 || !strings.Contains(fmt.Sprint(toasts), "lent agent-10 to pal-desktop") {
		t.Errorf("no toast for the lend: %v", toasts)
	}
}

func TestRevokeAsksFirst(t *testing.T) {
	c, f, tab := tabOpenAccess(t)
	tab.Update(tabKey("down"), c)
	if cmd := tab.Update(tabKey("x"), c); cmd != nil && len(f.Calls) > 0 {
		t.Fatalf("x revoked at once: %v", f.Calls)
	}
	if view := ansi.Strip(tab.View(c, 120, 30)); !strings.Contains(view, "Revoke friend-laptop?") {
		t.Errorf("no question:\n%s", view)
	}
	if !tab.Capturing() {
		t.Errorf("the question does not capture keys")
	}
	tab.Update(tabKey("n"), c)
	if len(f.Calls) > 0 || tab.Capturing() {
		t.Errorf("n still revoked: %v", f.Calls)
	}
	tab.Update(tabKey("x"), c)
	tabRun(tab.Update(tabKey("y"), c))
	if !slices.Equal(f.Calls, []string{"access revoke nST2 "}) {
		t.Errorf("calls %v, want one revoke of nST2", f.Calls)
	}
}

func TestAdminAsksFirstToo(t *testing.T) {
	c, f, tab := tabOpenAccess(t)
	tab.Update(tabKey("down"), c)
	tab.Update(tabKey("down"), c) // the knock
	tab.Update(tabKey("a"), c)
	if len(f.Calls) > 0 || !strings.Contains(ansi.Strip(tab.View(c, 120, 30)), "Make pal-desktop an admin?") {
		t.Errorf("admin did not ask first: %v", f.Calls)
	}
	tab.Update(tabKey("esc"), c)
	if len(f.Calls) > 0 {
		t.Errorf("esc made it an admin: %v", f.Calls)
	}
}

// wideScreens is the Fake, whose screens are full of wide runes, tabs and
// lines far too long for any tile.
type wideScreens struct{ *fleetapi.Fake }

func (w wideScreens) Screens(ctx context.Context, workers []string, lines int) ([]fleetapi.Screen, error) {
	var out []fleetapi.Screen
	for _, wk := range workers {
		out = append(out, fleetapi.Screen{Worker: wk, Text: strings.Join([]string{
			"\x1b[1m修复登录页面🚀🚀 and a very long line that runs well past the edge of any tile at all\x1b[0m",
			"\ttabbed\tout\x07 with a bell",
			strings.Repeat("界", 80),
			"é combining, 👩‍💻 joined, ｆｕｌｌ width",
		}, "\n"), At: tabsNow})
	}
	return out, nil
}

func TestTilesKeepTheirWidthWithWideRunes(t *testing.T) {
	c, f := tabCtx(t, fleetapi.RoleAdmin)
	c.Client = wideScreens{f}
	c.Snap.Workers[0].Task = "修复登录🚀-bug-with-a-long-name"
	for _, sz := range [][2]int{{120, 40}, {61, 9}, {59, 20}, {200, 60}, {80, 24}} {
		tab := newScreens()
		tabSettle(tab, c, sz[0], sz[1])
		tabSize(t, "grid", tab.View(c, sz[0], sz[1]), sz[0], sz[1])
		tab.Update(tabKey("enter"), c)
		tabSettle(tab, c, sz[0], sz[1])
		tabSize(t, "zoom", tab.View(c, sz[0], sz[1]), sz[0], sz[1])
	}
}

func TestTheTabsKeepTheirSizeOnAnyTerminal(t *testing.T) {
	sizes := [][2]int{{0, 0}, {1, 1}, {20, 5}, {40, 12}, {80, 24}}
	for _, role := range []fleetapi.Role{fleetapi.RoleAdmin, fleetapi.RoleGuest} {
		c, _ := tabCtx(t, role)
		full := c.Snap
		for _, snap := range []*fleetapi.Snapshot{nil, {}, full} {
			c.Snap = snap
			for _, tab := range []Tab{newScreens(), newQuota(), newHosts(), newAccess()} {
				for _, sz := range sizes {
					what := fmt.Sprintf("%s (%s, snapshot %v)", tab.Title(), role, snap != nil)
					tabSize(t, what, tab.View(c, sz[0], sz[1]), sz[0], sz[1])
					tabFeed(tab, c, tabRun(tab.Update(FrameMsg{}, c)))
					for _, k := range []string{"down", "right", "enter", "l", "u", "a", "esc", "up", "left"} {
						tab.Update(tabKey(k), c) // nothing that runs, but nothing that panics
					}
					tabSize(t, what+" after keys", tab.View(c, sz[0], sz[1]), sz[0], sz[1])
				}
			}
		}
	}
}

func TestGoldenTabViews(t *testing.T) {
	for _, role := range []fleetapi.Role{fleetapi.RoleAdmin, fleetapi.RoleGuest} {
		for _, mk := range []func() Tab{newScreens, newQuota, newHosts, newAccess} {
			for _, sz := range [][2]int{{80, 24}, {120, 40}, {200, 60}} {
				c, _ := tabCtx(t, role)
				tab := mk()
				if !tab.Visible(c.Snap.Me) {
					continue
				}
				tabSettle(tab, c, sz[0], sz[1])
				view := tab.View(c, sz[0], sz[1])
				tabSize(t, tab.Title(), view, sz[0], sz[1])
				name := fmt.Sprintf("%s_%s_%dx%d.golden", strings.ToLower(tab.Title()), role, sz[0], sz[1])
				tabGolden(t, name, ansi.Strip(view)+"\n")
			}
		}
	}
}

func tabGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if rewriteGolden() {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to write it)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file; run go test -update and review the diff:\n%s", name, got)
	}
}
