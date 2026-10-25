package tui

import (
	"fmt"
	"image/color"
	"math"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Shared drawing helpers. Add new ones at the end; do not change these
// signatures (both TUI tasks use them).

// Bar is a w-cell bar for pct (0–100+), colored by how full it is.
func Bar(t *Theme, pct float64, w int) string {
	if w <= 0 {
		return ""
	}
	p := math.Max(0, math.Min(pct, 100)) / 100
	full := int(math.Round(p * float64(w)))
	var c color.Color = t.Cyan
	switch {
	case pct >= 90:
		c = t.Alert
	case pct >= 70:
		c = t.Amber
	}
	return lipgloss.NewStyle().Foreground(c).Render(strings.Repeat("█", full)) +
		lipgloss.NewStyle().Foreground(t.Line).Render(strings.Repeat("░", w-full))
}

var sparks = []rune("▁▂▃▄▅▆▇█")

// Spark is a w-cell sparkline of the last w points, scaled to their range
// (or to [0, max] when max > 0).
func Spark(t *Theme, pts []fleetapi.Point, w int, max float64, c color.Color) string {
	if w <= 0 {
		return ""
	}
	if len(pts) > w {
		pts = pts[len(pts)-w:]
	}
	lo, hi := 0.0, max
	if max <= 0 {
		lo, hi = math.Inf(1), math.Inf(-1)
		for _, p := range pts {
			lo, hi = math.Min(lo, p.V), math.Max(hi, p.V)
		}
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", w-len(pts)))
	for _, p := range pts {
		i := 0
		if hi > lo {
			i = int(math.Round((math.Min(p.V, hi) - lo) / (hi - lo) * float64(len(sparks)-1)))
		}
		b.WriteRune(sparks[max0(i)])
	}
	return lipgloss.NewStyle().Foreground(c).Render(b.String())
}

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}

// Age is a short duration: "40s", "6m", "6h11m", "2d3h"; "—" when unknown.
func Age(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(d / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 86400:
		if m := (s % 3600) / 60; m > 0 {
			return fmt.Sprintf("%dh%dm", s/3600, m)
		}
		return fmt.Sprintf("%dh", s/3600)
	}
	return fmt.Sprintf("%dd%dh", s/86400, (s%86400)/3600)
}

// Since is Age(now − at), or "—" for a zero time.
func Since(now, at time.Time) string {
	if at.IsZero() {
		return "—"
	}
	return Age(now.Sub(at))
}

// Bytes is a size in the largest unit that keeps it short: 512M, 2.4G.
func Bytes(n int64) string {
	switch {
	case n >= 10<<30:
		return fmt.Sprintf("%dG", n>>30)
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n>>20)
	}
	return fmt.Sprintf("%dK", n>>10)
}

// Fit cuts s (which may hold ANSI styles) to w cells, with … when cut, and
// pads it to exactly w.
func Fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", max(0, w-lipgloss.Width(s)))
}
