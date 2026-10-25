package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

func TestSamplesKeepOneEveryTenSecondsForHalfAnHour(t *testing.T) {
	var s Samples
	s.Add(testNow, 2)
	s.Add(testNow.Add(4*time.Second), 5) // within 10 s: raises the last
	s.Add(testNow.Add(9*time.Second), 1)
	if got := s.Points(); len(got) != 1 || got[0].V != 5 {
		t.Fatalf("three snapshots in 10 s left %v", got)
	}
	for i := 1; i <= 200; i++ {
		s.Add(testNow.Add(time.Duration(i)*pulseEvery), float64(i))
	}
	got := s.Points()
	if len(got) != int(pulseSpan/pulseEvery) {
		t.Fatalf("the ring holds %d samples", len(got))
	}
	if first, last := got[0], got[len(got)-1]; last.V != 200 || last.T.Sub(first.T) >= pulseSpan {
		t.Errorf("the ring spans %v to %v", first, last)
	}
}

// pulseApp is an admin app at w×h that has seen half an hour of snapshots,
// a minute apart, with these working counts, then the Fake's own.
func pulseApp(t *testing.T, w, h int, counts []int) *App {
	t.Helper()
	f := fleetapi.NewFake(fleetapi.RoleAdmin, clock)
	now := testNow.Add(-time.Duration(len(counts)) * time.Minute)
	a := newApp(context.Background(), Options{Client: f, Version: "v0.3.0", NoAnim: true, Now: func() time.Time { return now }})
	a.Init()
	a.Update(tea.WindowSizeMsg{Width: w, Height: h})
	for _, n := range counts {
		s := &fleetapi.Snapshot{}
		for range n {
			s.Workers = append(s.Workers, fleetapi.Worker{State: "working"})
		}
		a.Update(SnapshotMsg{Snap: s})
		now = now.Add(time.Minute)
	}
	now = testNow
	snap(t, a, f)
	return a
}

var pulseCounts = []int{1, 1, 2, 2, 2, 3, 4, 4, 5, 5, 4, 3, 3, 2, 2, 3, 4, 6, 6, 5, 4, 3, 3, 3, 2, 2, 3, 3, 3}

func TestThePulseFillsTheRoomUnderTheCards(t *testing.T) {
	a := pulseApp(t, 140, 42, pulseCounts)
	v := view(a)
	checkSize(t, v, 140, 42)
	golden(t, "fleet_pulse_admin_140x42", v)
	plain := ansi.Strip(v)
	for _, want := range []string{"Working · last 30m", "7 tasks closed", "$41.37 spent", "1.28M tokens out",
		"Running out", "acct-02", "Events · latest", "question · agent-04", "now 3 · peak 6"} {
		if !strings.Contains(plain, want) {
			t.Errorf("the pulse has no %q", want)
		}
	}
}

func TestThePulseGoesWhenTheBodyIsShort(t *testing.T) {
	// The cards take 23 rows: at 140x34 the body (28) leaves 5 spare, at
	// 140x35 it leaves 6.
	for _, c := range []struct {
		h    int
		show bool
	}{{34, false}, {35, true}, {30, false}} {
		a := pulseApp(t, 140, c.h, pulseCounts)
		v := ansi.Strip(view(a))
		checkSize(t, view(a), 140, c.h)
		if got := strings.Contains(v, "Working · last 30m"); got != c.show {
			t.Errorf("at 140x%d the pulse shows: %v, want %v", c.h, got, c.show)
		}
		if !strings.Contains(v, "5h █") {
			t.Errorf("at 140x%d the cards went dense:\n%s", c.h, v)
		}
	}
}

func TestThePulseStartsEmpty(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 140, 42, false)
	if !strings.Contains(ansi.Strip(view(a)), "connecting") {
		t.Fatal("no wait for the first snapshot")
	}
	snap(t, a, f)
	if pts := a.c.Working.Points(); len(pts) != 1 || pts[0].V != 3 {
		t.Errorf("the first snapshot left %v", pts)
	}
	checkSize(t, view(a), 140, 42)
}

func TestThePulseKeepsCountingOnOtherTabs(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 140, 42, false)
	press(a, "4")
	snap(t, a, f)
	if len(a.c.Working.Points()) != 1 {
		t.Error("a snapshot seen on the Events tab was not counted")
	}
}
