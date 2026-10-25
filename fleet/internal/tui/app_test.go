package tui

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// update rewrites the golden files: go test ./internal/tui -update.
var update = flag.Bool("update", false, "rewrite the golden files")

// testNow is the fixed clock every TUI test runs at.
var testNow = time.Date(2026, 10, 25, 21, 4, 0, 0, time.UTC)

func clock() time.Time { return testNow }

// testApp is an app on a Fake fleet, sized, with no boot unless anim.
func testApp(t *testing.T, role fleetapi.Role, w, h int, anim bool) (*App, *fleetapi.Fake) {
	t.Helper()
	f := fleetapi.NewFake(role, clock)
	a := newApp(context.Background(), Options{Client: f, Version: "v0.3.0", NoAnim: !anim, Now: clock})
	a.Init()
	a.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return a, f
}

// snap delivers the Fake's current snapshot, as the watch loop would.
func snap(t *testing.T, a *App, f *fleetapi.Fake) {
	t.Helper()
	s, err := f.Watch(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	a.Update(SnapshotMsg{Snap: s})
}

func press(a *App, keys ...string) {
	for _, k := range keys {
		a.Update(keyMsg(k))
	}
}

// keyMsg is the key press bubbletea delivers for a key's name.
func keyMsg(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+s":
		return tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	}
	r := []rune(k)
	return tea.KeyPressMsg{Code: r[0], Text: k}
}

// typeText types s one key at a time.
func typeText(a *App, s string) {
	for _, r := range s {
		if r == ' ' {
			a.Update(keyMsg("space"))
			continue
		}
		a.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func view(a *App) string { return a.View().Content }

func TestBootSkipsOnAKey(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, true)
	if !a.booting {
		t.Fatal("the app did not boot")
	}
	if v := ansi.Strip(view(a)); !strings.Contains(v, "██") {
		t.Errorf("the boot screen has no wordmark:\n%s", v)
	}
	snap(t, a, f)
	press(a, "x")
	if a.booting {
		t.Fatal("a key did not skip the boot")
	}
	if a.active != 0 {
		t.Errorf("the skipping key did something else: tab %d", a.active)
	}
	if v := ansi.Strip(view(a)); !strings.Contains(v, "KARMAX ▸ fleetctl") {
		t.Errorf("after the boot there is no frame:\n%s", v)
	}
}

func TestTheBootEndsByItself(t *testing.T) {
	a, _ := testApp(t, fleetapi.RoleAdmin, 120, 40, true)
	for i := 0; i < bootFrames+1; i++ {
		a.Update(bootTickMsg{})
	}
	if a.booting {
		t.Error("the boot did not end after its frames")
	}
}

func TestNoAnimHasNoBoot(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	if a.booting {
		t.Fatal("NoAnim booted")
	}
	snap(t, a, f)
	a.Update(FrameMsg{})
	if a.c.Frame != 0 {
		t.Errorf("NoAnim moved the frame to %d", a.c.Frame)
	}
}

func TestTabsSwitchByNumberAndTab(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	title := func() string { return a.current().Title() }
	if title() != "Fleet" {
		t.Fatalf("starts on %s", title())
	}
	press(a, "3")
	if title() != "Tasks" {
		t.Errorf("3 is %s", title())
	}
	press(a, "tab")
	if title() != "Events" {
		t.Errorf("tab after Tasks is %s", title())
	}
	press(a, "shift+tab", "shift+tab", "shift+tab", "shift+tab")
	if title() != "Access" {
		t.Errorf("shift+tab wraps to %s", title())
	}
	press(a, "7")
	if title() != "Access" {
		t.Errorf("7 is %s", title())
	}
	press(a, "9")
	if title() != "Access" {
		t.Errorf("9, past the last tab, moved to %s", title())
	}
}

func TestATabIsToldWhenItIsShown(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	spy := &spyTab{}
	a.tabs = append(a.tabs, spy)
	press(a, "8")
	if len(spy.got) == 0 {
		t.Fatal("the shown tab got no message")
	}
	if _, ok := spy.got[0].(FrameMsg); !ok {
		t.Errorf("the shown tab first got %T, want FrameMsg", spy.got[0])
	}
}

type spyTab struct {
	stubTab
	got []tea.Msg
}

func (s *spyTab) Title() string                    { return "Spy" }
func (s *spyTab) Visible(fleetapi.Me) bool         { return true }
func (s *spyTab) Update(m tea.Msg, _ *Ctx) tea.Cmd { s.got = append(s.got, m); return nil }

func TestQuitKeys(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c"} {
		a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
		snap(t, a, f)
		_, cmd := a.Update(keyMsg(k))
		if cmd == nil {
			t.Fatalf("%s returned no command", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%s does not quit", k)
		}
	}
}

func TestHelpOpensAndCloses(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	press(a, "?")
	if v := ansi.Strip(view(a)); !strings.Contains(v, "switch tabs") {
		t.Errorf("? shows no help:\n%s", v)
	}
	press(a, "x")
	if a.help {
		t.Error("a key did not close the help")
	}
}

func TestAGuestHasNoHostsOrAccessTab(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleGuest, 120, 40, false)
	snap(t, a, f)
	var titles []string
	for _, tb := range a.visible() {
		titles = append(titles, tb.Title())
	}
	if got := strings.Join(titles, " "); got != "Fleet Screens Tasks Events Quota" {
		t.Errorf("a guest's tabs are %s", got)
	}
	bar := ansi.Strip(strings.Split(view(a), "\n")[1])
	if strings.Contains(bar, "Hosts") || strings.Contains(bar, "Access") {
		t.Errorf("the tab bar shows a guest Hosts or Access: %s", bar)
	}
	press(a, "6")
	if a.current().Title() != "Fleet" {
		t.Errorf("6 took a guest to %s", a.current().Title())
	}
}

func TestAWatchErrorKeepsTheLastSnapshotAndShowsABanner(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	_, cmd := a.Update(WatchErrMsg{Err: errors.New("dial tcp 100.64.0.7:7878: connection refused")})
	if a.c.Snap == nil {
		t.Fatal("the error dropped the snapshot")
	}
	if cmd == nil {
		t.Error("no retry was scheduled")
	}
	if a.back != time.Second {
		t.Errorf("the first retry waits %v, want 1s", a.back)
	}
	v := ansi.Strip(view(a))
	if !strings.Contains(v, "fleetd unreachable") || !strings.Contains(v, "agent-01") {
		t.Errorf("want a banner over the last snapshot:\n%s", v)
	}
	for i := 0; i < 6; i++ {
		a.Update(WatchErrMsg{Err: errors.New("still down")})
	}
	if a.back != 15*time.Second {
		t.Errorf("the backoff grew to %v, want at most 15s", a.back)
	}
	snap(t, a, f)
	if a.err != nil || a.back != 0 {
		t.Error("a snapshot did not clear the error and the backoff")
	}
	if strings.Contains(ansi.Strip(view(a)), "fleetd unreachable") {
		t.Error("the banner stayed after the fleet came back")
	}
}

func TestANilSnapshotKeepsTheLastOne(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	a.Update(SnapshotMsg{})
	_ = view(a)
	snap(t, a, f)
	a.Update(SnapshotMsg{})
	if a.c.Snap == nil {
		t.Error("a nil snapshot replaced the last one")
	}
}

func TestTheWatchLoopFetchesSinceTheLastSeq(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	msg := a.watch(0)()
	s, ok := msg.(SnapshotMsg)
	if !ok || s.Snap == nil {
		t.Fatalf("the first watch answered %T", msg)
	}
	_, cmd := a.Update(s)
	if cmd == nil {
		t.Fatal("no next watch after a snapshot")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- a.watch(a.since)() }()
	select {
	case m := <-done:
		t.Fatalf("the next watch did not wait for a change: %T", m)
	case <-time.After(20 * time.Millisecond):
	}
	f.Step()
	if m := <-done; m.(SnapshotMsg).Snap.Seq != s.Snap.Seq+1 {
		t.Errorf("the next watch returned seq %d", m.(SnapshotMsg).Snap.Seq)
	}
}

// eachSize is the sizes every frame test walks: tiny, odd and usual.
var eachSize = [][2]int{{0, 0}, {1, 1}, {20, 5}, {40, 12}, {79, 23}, {80, 24}, {120, 40}, {200, 60}}

func TestTinyTerminalsDoNotPanic(t *testing.T) {
	for _, role := range []fleetapi.Role{fleetapi.RoleAdmin, fleetapi.RoleGuest} {
		for _, sz := range eachSize {
			for _, anim := range []bool{false, true} {
				a, f := testApp(t, role, sz[0], sz[1], anim)
				_ = view(a)
				snap(t, a, f)
				press(a, "x")
				for range a.visible() {
					_ = view(a)
					checkSize(t, view(a), sz[0], sz[1])
					press(a, "enter")
					_ = view(a)
					press(a, "esc", "tab")
				}
			}
		}
	}
}

func TestAnEmptyFleetDraws(t *testing.T) {
	a, _ := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	a.Update(SnapshotMsg{Snap: &fleetapi.Snapshot{Seq: 1, At: testNow, Me: fleetapi.Me{Name: "operator", Role: fleetapi.RoleAdmin}}})
	for range a.visible() {
		checkSize(t, view(a), 120, 40)
		press(a, "enter", "esc", "tab")
	}
}

func TestWideRunesInATaskNameKeepTheFrame(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}, {200, 60}} {
		a, f := testApp(t, fleetapi.RoleAdmin, sz[0], sz[1], false)
		s, _ := f.Watch(context.Background(), 0)
		wide := "20261025-修复登录-🚀🚀-ｗｉｄｅ-é"
		for i := range s.Workers {
			if s.Workers[i].Task != "" {
				s.Workers[i].Task = wide
			}
		}
		for i := range s.Tasks {
			s.Tasks[i].ID, s.Tasks[i].Last = wide, "修复登录 🚀 "+s.Tasks[i].Last
		}
		for i := range s.Events {
			s.Events[i].Detail = []byte(`{"text":"修复登录 🚀 ｗｉｄｅ"}`)
		}
		s.Me.Device = "瑞-mac.tail1234.ts.net"
		a.Update(SnapshotMsg{Snap: s})
		for range a.visible() {
			checkSize(t, view(a), sz[0], sz[1])
			press(a, "tab")
		}
	}
}

// checkSize fails unless v is exactly w×h cells: every line w wide.
func checkSize(t *testing.T, v string, w, h int) {
	t.Helper()
	if w <= 0 || h <= 0 {
		if v != "" {
			t.Errorf("%dx%d: want nothing drawn, got %q", w, h, v)
		}
		return
	}
	lines := strings.Split(v, "\n")
	if len(lines) != h {
		t.Errorf("%dx%d: %d lines", w, h, len(lines))
	}
	for i, l := range lines {
		if got := lipgloss.Width(l); got != w {
			t.Errorf("%dx%d: line %d is %d wide: %q", w, h, i, got, ansi.Strip(l))
			return
		}
	}
}

// golden compares the ANSI-stripped view with testdata/<name>.golden, or
// writes it under -update.
func golden(t *testing.T, name, v string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	got := ansi.Strip(v) + "\n"
	if *update {
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
		t.Fatalf("%v (run go test -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file (go test -update rewrites it):\n%s", name, got)
	}
}

func TestGoldenViews(t *testing.T) {
	for _, role := range []fleetapi.Role{fleetapi.RoleAdmin, fleetapi.RoleGuest} {
		for _, sz := range [][2]int{{80, 24}, {120, 40}, {200, 60}} {
			for _, tab := range []struct{ key, name string }{{"1", "fleet"}, {"3", "tasks"}, {"4", "events"}} {
				a, f := testApp(t, role, sz[0], sz[1], false)
				snap(t, a, f)
				press(a, tab.key)
				name := fmt.Sprintf("%s_%s_%dx%d", tab.name, role, sz[0], sz[1])
				v := view(a)
				checkSize(t, v, sz[0], sz[1])
				golden(t, name, v)
			}
		}
	}
}
