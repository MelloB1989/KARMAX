package tui

import (
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

func TestWidgetsKeepTheirWidth(t *testing.T) {
	th := NewTheme(true)
	for _, w := range []int{1, 5, 12} {
		for _, pct := range []float64{-5, 0, 42, 100, 140} {
			if got := lipgloss.Width(Bar(th, pct, w)); got != w {
				t.Errorf("Bar(%v, %d) is %d wide", pct, w, got)
			}
		}
		pts := []fleetapi.Point{{V: 1}, {V: 3}, {V: 2}}
		if got := lipgloss.Width(Spark(th, pts, w, 0, th.Cyan)); got != w {
			t.Errorf("Spark(%d) is %d wide", w, got)
		}
		if got := lipgloss.Width(Fit("a much longer line than fits", w)); got != w {
			t.Errorf("Fit(%d) is %d wide", w, got)
		}
	}
	for k := range map[fleetapi.ActivityKind]bool{fleetapi.ActReading: true, fleetapi.ActPushing: true, fleetapi.ActIdle: true, "": true} {
		if got := lipgloss.Width(Sprite(th, k, 3)); got != SpriteWidth {
			t.Errorf("Sprite(%q) is %d wide", k, got)
		}
	}
}

func TestAgeIsShort(t *testing.T) {
	for d, want := range map[time.Duration]string{40 * time.Second: "40s", 6*time.Minute + 5*time.Second: "6m",
		6*time.Hour + 11*time.Minute: "6h11m", 3 * time.Hour: "3h", 51 * time.Hour: "2d3h", -time.Second: "0s"} {
		if got := Age(d); got != want {
			t.Errorf("Age(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestPhaseFollowsTheConsole(t *testing.T) {
	th := NewTheme(true)
	if c, l := th.Phase("working", []string{"stuck-tool"}); l != "stuck" || c != th.Alert {
		t.Errorf("stuck: %v %s", c, l)
	}
	if _, l := th.Phase("waiting", nil); l != "waiting" {
		t.Errorf("waiting: %s", l)
	}
}
