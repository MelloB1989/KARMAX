package tui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Theme is the console's palette (fleet/console/src/state/derive.ts) for a
// dark terminal, and a readable counterpart for a light one.
type Theme struct {
	Dark                                                  bool
	Ground, Panel, Line, Amber, Cyan, Text, Dim, Alert    color.Color
	Frost, Grey, Green, Violet                            color.Color
	Brand                                                 []color.Color // the KARMAX wordmark's gradient stops
	Title, Muted, Strong, Border, Selected, Danger, Badge lipgloss.Style
}

// NewTheme is the theme for a dark (or light) terminal background.
func NewTheme(dark bool) *Theme {
	ld := lipgloss.LightDark(dark)
	c := func(light, darkHex string) color.Color { return ld(lipgloss.Color(light), lipgloss.Color(darkHex)) }
	t := &Theme{
		Dark:   dark,
		Ground: c("#F7F9FB", "#05080C"),
		Panel:  c("#EEF2F6", "#0A1018"),
		Line:   c("#97A5B3", "#1B2A38"),
		Amber:  c("#B86E00", "#FFB547"),
		Cyan:   c("#007C82", "#5CE1E6"),
		Text:   c("#1B2733", "#D6E2EC"),
		Dim:    c("#5F7080", "#7F93A6"),
		Alert:  c("#C62828", "#FF5D5D"),
		Frost:  c("#1F6FB2", "#8EC9FF"),
		Grey:   c("#7B8794", "#5B6773"),
		Green:  c("#2E7D32", "#7BE495"),
		Violet: c("#6A3FB5", "#B79CFF"),
	}
	t.Brand = []color.Color{t.Amber, c("#C2410C", "#FF7A59"), t.Violet, t.Cyan}
	t.Title = lipgloss.NewStyle().Foreground(t.Text).Bold(true)
	t.Muted = lipgloss.NewStyle().Foreground(t.Dim)
	t.Strong = lipgloss.NewStyle().Foreground(t.Text).Bold(true)
	t.Border = lipgloss.NewStyle().Foreground(t.Line)
	t.Selected = lipgloss.NewStyle().Foreground(t.Amber).Bold(true)
	t.Danger = lipgloss.NewStyle().Foreground(t.Alert).Bold(true)
	t.Badge = lipgloss.NewStyle().Foreground(t.Ground).Background(t.Amber).Bold(true).Padding(0, 1)
	return t
}

// Phase is a worker state's color and label, as the console's phaseLook:
// a stuck alert wins over the phase.
func (t *Theme) Phase(state string, alerts []string) (color.Color, string) {
	for _, a := range alerts {
		if strings.HasPrefix(a, "stuck") {
			return t.Alert, "stuck"
		}
	}
	switch state {
	case "working":
		return t.Amber, "working"
	case "waiting":
		return t.Cyan, "waiting"
	case "standby":
		return t.Dim, "standby"
	case "cooling":
		return t.Frost, "cooling"
	case "disabled":
		return t.Grey, "disabled"
	case "down", "code_missing":
		return t.Alert, state
	}
	if state == "" {
		return t.Dim, "unknown"
	}
	return t.Dim, state
}

// Activity is an activity's color.
func (t *Theme) Activity(k fleetapi.ActivityKind) color.Color {
	switch k {
	case fleetapi.ActReading:
		return t.Cyan
	case fleetapi.ActEditing, fleetapi.ActRunning:
		return t.Amber
	case fleetapi.ActPushing, fleetapi.ActTesting:
		return t.Green
	case fleetapi.ActThinking:
		return t.Violet
	case fleetapi.ActWaiting:
		return t.Cyan
	case fleetapi.ActCooling:
		return t.Frost
	case fleetapi.ActDown:
		return t.Alert
	}
	return t.Dim
}

// Gradient paints s with the brand gradient, one color per rune.
func (t *Theme) Gradient(s string, bold bool) string {
	rs := []rune(s)
	if len(rs) == 0 {
		return ""
	}
	cols := lipgloss.Blend1D(len(rs), t.Brand...)
	var b strings.Builder
	for i, r := range rs {
		st := lipgloss.NewStyle().Foreground(cols[i])
		if bold {
			st = st.Bold(true)
		}
		b.WriteString(st.Render(string(r)))
	}
	return b.String()
}
