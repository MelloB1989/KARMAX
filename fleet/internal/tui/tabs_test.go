package tui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// do runs the command a key returned, as bubbletea would, and delivers what
// it says back to the app. It does not follow the commands those return
// (a watch would block).
func do(a *App, cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	var msgs []tea.Msg
	switch m := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range m {
			msgs = append(msgs, do(a, c)...)
		}
	default:
		a.Update(m)
		msgs = append(msgs, m)
	}
	return msgs
}

// pressDo presses a key and runs what it returns.
func pressDo(a *App, k string) []tea.Msg {
	_, cmd := a.Update(keyMsg(k))
	return do(a, cmd)
}

func calls(f *fleetapi.Fake) []string { return slices.Clone(f.Calls) }

func TestRunFormSendsTheRequest(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	press(a, "3", "r")
	if !a.current().Capturing() {
		t.Fatal("the run form does not hold the keys")
	}
	typeText(a, "fix the login page")
	press(a, "tab")
	typeText(a, "KARMAX")
	press(a, "tab", "right", "right") // any → agent-01 → agent-02
	press(a, "tab", "right")          // the agent's default → sonnet
	msgs := pressDo(a, "ctrl+s")
	want := `run agent-02 "fix the login page"`
	if got := calls(f); len(got) != 1 || got[0] != want {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0].(ToastMsg).Text, "task 20261025-task-6 → agent-02") {
		t.Errorf("the toast is %v", msgs)
	}
	if a.current().Capturing() {
		t.Error("the form stayed open after sending")
	}
	tasks, _ := f.Watch(t.Context(), 0)
	if got := tasks.Tasks[0]; got.Repo != "KARMAX" || got.Model != "sonnet" {
		t.Errorf("the task went with repo %q, model %q", got.Repo, got.Model)
	}
}

func TestAnEmptyBriefIsNotSent(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	press(a, "3", "r")
	msgs := pressDo(a, "ctrl+s")
	if len(calls(f)) != 0 {
		t.Errorf("an empty brief was sent: %q", calls(f))
	}
	if len(msgs) == 0 || !msgs[len(msgs)-1].(ToastMsg).Err {
		t.Errorf("no error toast: %v", msgs)
	}
	if !a.current().Capturing() {
		t.Error("the form closed on an empty brief")
	}
}

func TestAGuestRunsOnlyOnItsLentAgents(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleGuest, 120, 40, false)
	snap(t, a, f)
	press(a, "3", "r")
	form := a.current().(*tasksTab).form
	if got := strings.Join(form.agents, ","); got != ",agent-09" {
		t.Errorf("a guest's agent choices are %q", got)
	}
}

// selectTask moves the Tasks tab's selection to the task with this id.
func selectTask(t *testing.T, a *App, id string) {
	t.Helper()
	press(a, "3")
	for range 10 {
		tb := a.current().(*tasksTab)
		if x, ok := tb.selected(&a.c); ok && x.ID == id {
			return
		}
		press(a, "down")
	}
	t.Fatalf("no task %s in the list", id)
}

func TestAnswerOnlyOnAQuestion(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	selectTask(t, a, "20261025-tui-frame-1a2b") // its last message is progress
	msgs := pressDo(a, "a")
	if a.current().Capturing() || len(msgs) == 0 || !msgs[0].(ToastMsg).Err {
		t.Fatalf("a on a task with no question opened an answer: %v", msgs)
	}
	selectTask(t, a, "20261025-access-3c4d") // agent-04 asked
	press(a, "a")
	if !a.current().Capturing() {
		t.Fatal("a on a question opened nothing")
	}
	typeText(a, "refuse it")
	pressDo(a, "enter")
	if got := calls(f); len(got) != 1 || got[0] != `answer 20261025-access-3c4d "refuse it"` {
		t.Errorf("calls %q", got)
	}
}

func TestFollowUpSendsTheText(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	selectTask(t, a, "20261025-term-5e6f")
	press(a, "f")
	typeText(a, "also Windows")
	pressDo(a, "enter")
	if got := calls(f); len(got) != 1 || got[0] != `followup 20261025-term-5e6f "also Windows"` {
		t.Errorf("calls %q", got)
	}
}

func TestDoneAsksFirst(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	selectTask(t, a, "20261025-term-5e6f")
	pressDo(a, "d")
	if len(calls(f)) != 0 {
		t.Fatalf("d closed the task without asking: %q", calls(f))
	}
	if v := ansi.Strip(view(a)); !strings.Contains(v, "Close 20261025-term-5e6f on agent-05:2 as done?") {
		t.Errorf("no question shown:\n%s", v)
	}
	pressDo(a, "n")
	if len(calls(f)) != 0 {
		t.Fatalf("n closed the task: %q", calls(f))
	}
	pressDo(a, "d")
	pressDo(a, "y")
	if got := calls(f); len(got) != 1 || got[0] != "done 20261025-term-5e6f" {
		t.Errorf("calls %q", got)
	}
}

func TestCancelFromTheFleetTabAsksFirst(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	press(a, "1")
	if msgs := pressDo(a, "c"); len(calls(f)) != 0 || len(msgs) != 0 {
		t.Fatalf("c cancelled without asking: %q", calls(f))
	}
	pressDo(a, "y")
	if got := calls(f); len(got) != 1 || !strings.HasPrefix(got[0], "cancel 20261025-tui-frame-1a2b") {
		t.Errorf("calls %q", got)
	}
	press(a, "down")
	if msgs := pressDo(a, "d"); len(msgs) != 1 || !msgs[0].(ToastMsg).Err {
		t.Errorf("d on a worker with no task: %v", msgs)
	}
}

func TestModelIsForAdmins(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	press(a, "m", "left")
	pressDo(a, "enter")
	if got := calls(f); len(got) != 1 || got[0] != "model agent-01 sonnet" {
		t.Errorf("calls %q", got)
	}
	g, gf := testApp(t, fleetapi.RoleGuest, 120, 40, false)
	snap(t, g, gf)
	if msgs := pressDo(g, "m"); len(msgs) != 1 || !msgs[0].(ToastMsg).Err {
		t.Errorf("a guest's m: %v", msgs)
	}
}

// On the Fleet tab s is a shell in the agent's container; t types into its
// Claude Code session (watching a session is the Screens tab's s).
func TestSSHKeysAttachTheSelectedWorker(t *testing.T) {
	f := fleetapi.NewFake(fleetapi.RoleAdmin, clock)
	var got []string
	a := newApp(t.Context(), Options{Client: f, NoAnim: true, Now: clock, Attach: func(w string, m fleetapi.TermMode) tea.ExecCommand {
		got = append(got, w+" "+string(m))
		return nil
	}})
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	snap(t, a, f)
	press(a, "down")
	a.Update(keyMsg("s"))
	a.Update(keyMsg("t"))
	if strings.Join(got, ",") != "agent-01:2 shell,agent-01:2 type" {
		t.Errorf("attached %q", got)
	}
}

func TestTheTaskDetailShowsTheThread(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	selectTask(t, a, "20261025-access-3c4d")
	pressDo(a, "enter")
	v := ansi.Strip(view(a))
	for _, want := range []string{"the brief for 20261025-access-3c4d", "Should a lend to an admin device", "question"} {
		if !strings.Contains(v, want) {
			t.Errorf("the thread lacks %q:\n%s", want, v)
		}
	}
	press(a, "esc")
	if a.current().(*tasksTab).detail != nil {
		t.Error("esc left the thread open")
	}
}

func TestEventsFilterByAgent(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	press(a, "4", "/")
	if !a.current().Capturing() {
		t.Fatal("/ opened no filter")
	}
	typeText(a, "agent-04")
	press(a, "enter")
	tb := a.current().(*eventsTab)
	evs := tb.events(&a.c)
	if len(evs) != 1 || evs[0].Agent != "agent-04" {
		t.Fatalf("filtered to %v", evs)
	}
	press(a, "esc")
	if len(tb.events(&a.c)) != len(a.c.Snap.Events) {
		t.Error("esc did not clear the filter")
	}
	for q, want := range map[string]int{"kind:fleet.report": 2, "task:access": 1, "agent-05": 1, "agent-05:2": 1, "fleet.alert": 1, "agent-0": 0} {
		tb.filter.SetValue(q)
		if got := len(tb.events(&a.c)); got != want {
			t.Errorf("%q matched %d, want %d", q, got, want)
		}
	}
}

func TestSpacePausesTheEvents(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	press(a, "4", "space")
	s := *a.c.Snap
	s.Events = s.Events[:1]
	a.Update(SnapshotMsg{Snap: &s})
	tb := a.current().(*eventsTab)
	if got := len(tb.events(&a.c)); got != 7 {
		t.Errorf("paused, the stream changed to %d events", got)
	}
	if !strings.Contains(ansi.Strip(view(a)), "paused") {
		t.Error("the pause is not shown")
	}
	press(a, "space")
	if got := len(tb.events(&a.c)); got != 1 {
		t.Errorf("resumed, the stream has %d events", got)
	}
}

func TestDenseModeWhenAgentsDoNotFit(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 200, 60, false)
	snap(t, a, f)
	if v := ansi.Strip(view(a)); !strings.Contains(v, "5h ███") {
		t.Errorf("200x60 has room for the bars:\n%s", v)
	}
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v := ansi.Strip(view(a))
	if strings.Contains(v, "5h ███") || !strings.Contains(v, "5h 34% 7d 11%") {
		t.Errorf("120x40 has no room for bars, so the windows go in the header:\n%s", v)
	}
	if !strings.Contains(v, "agent-12") {
		t.Errorf("dense 120x40 does not show every agent:\n%s", v)
	}
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if v := ansi.Strip(view(a)); !strings.Contains(v, "more") {
		t.Errorf("80x24 does not say more agents are below:\n%s", v)
	}
	for range 25 {
		press(a, "down")
	}
	if v := ansi.Strip(view(a)); !strings.Contains(v, "▸ ") || !strings.Contains(v, "agent-12") {
		t.Errorf("the last worker is not scrolled into view:\n%s", v)
	}
}

func TestTheDetailPaneShowsTheWorker(t *testing.T) {
	for _, w := range []int{80, 120, 200} {
		a, f := testApp(t, fleetapi.RoleAdmin, w, 40, false)
		snap(t, a, f)
		press(a, "enter")
		v := ansi.Strip(view(a))
		for _, want := range []string{"agent-01", "tui-frame-1a2b", "resets", "Recent", "fleet.report"} {
			if !strings.Contains(v, want) {
				t.Errorf("%d wide: the detail lacks %q:\n%s", w, want, v)
			}
		}
		checkSize(t, view(a), w, 40)
	}
}

// fleetd's times come over the wire in UTC; the TUI shows them in the
// viewer's own zone, as its clock does.
func TestTimesShowInTheViewersZone(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("IST", 5*3600+1800)
	t.Cleanup(func() { time.Local = old })
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	s, err := f.Watch(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 25, 17, 37, 0, 0, time.UTC) // 23:07 in IST
	s.Events = []fleetapi.Event{{ID: 1, At: at, Agent: "agent-01", Kind: "fleet.report", Detail: []byte(`{"text":"hi"}`)}}
	s.Today.Since = time.Date(2026, 10, 24, 18, 30, 0, 0, time.UTC) // midnight in IST
	a.Update(SnapshotMsg{Snap: s})
	press(a, "4")
	if v := ansi.Strip(view(a)); !strings.Contains(v, "23:07:00") || strings.Contains(v, "17:37") {
		t.Errorf("an event at 17:37 UTC is not shown at 23:07 IST:\n%s", v)
	}
}
