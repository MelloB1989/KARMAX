package tui

import (
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Drawing helpers for the Screens, Quota, Hosts and Access tabs.

// fillBody is lines cut and padded to exactly w×h cells.
func fillBody(lines []string, w, h int) string {
	if h <= 0 {
		return ""
	}
	out := make([]string, h)
	for i := range out {
		if i < len(lines) {
			out[i] = Fit(lines[i], w)
		} else {
			out[i] = Fit("", w)
		}
	}
	return strings.Join(out, "\n")
}

// keyOf is a key press's name ("enter", "s", "up"), or "" for any other
// message.
func keyOf(msg tea.Msg) string {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		return k.String()
	}
	return ""
}

// toastCmd is a command that shows a notice in the footer.
func toastCmd(text string, isErr bool) tea.Cmd {
	return func() tea.Msg { return ToastMsg{Text: text, Err: isErr} }
}

// rawLine makes a line of captured terminal output safe to draw in a w-cell
// cell: tabs become spaces, other control bytes (but ESC) go, it is cut to w
// keeping its ANSI, and its style is reset so it cannot bleed into the frame.
func rawLine(s string, w int) string {
	s = strings.ReplaceAll(s, "\t", "    ")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != 0x1b || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "")
	}
	return s + "\x1b[0m" + strings.Repeat(" ", max(0, w-lipgloss.Width(s)))
}

// tileBox draws body in a rounded box of exactly w×h with title set into
// its top border; a heavy box marks the selected tile.
func tileBox(t *Theme, title string, c color.Color, heavy bool, body []string, w, h int) []string {
	if w < 2 || h < 2 {
		return strings.Split(fillBody(nil, w, h), "\n")
	}
	tl, tr, bl, br, hz, vt := "╭", "╮", "╰", "╯", "─", "│"
	if heavy {
		tl, tr, bl, br, hz, vt = "┏", "┓", "┗", "┛", "━", "┃"
	}
	bs := lipgloss.NewStyle().Foreground(c)
	in := w - 2
	top := bs.Render(strings.Repeat(hz, in))
	if in >= 5 && title != "" {
		if lipgloss.Width(title) > in-4 {
			title = ansi.Truncate(title, in-4, "…")
		}
		top = bs.Render(hz+" ") + title + bs.Render(" "+strings.Repeat(hz, in-3-lipgloss.Width(title)))
	}
	out := []string{bs.Render(tl) + top + bs.Render(tr)}
	for i := 0; i < h-2; i++ {
		line := ""
		if i < len(body) {
			line = body[i]
		}
		out = append(out, bs.Render(vt)+Fit(line, in)+bs.Render(vt))
	}
	return append(out, bs.Render(bl+strings.Repeat(hz, in)+br))
}

// centerNote is msg in the middle of a w×h body, dim.
func centerNote(t *Theme, msg string, w, h int) string {
	lines := make([]string, h/2)
	pad := max(0, (w-lipgloss.Width(msg))/2)
	return fillBody(append(lines, strings.Repeat(" ", pad)+t.Muted.Render(msg)), w, h)
}

// keepInView is the window of n rows of height h that keeps row sel in view,
// starting from off: the new offset.
func keepInView(off, sel, n, h int) int {
	if h <= 0 {
		return 0
	}
	if sel < off {
		off = sel
	}
	if sel >= off+h {
		off = sel - h + 1
	}
	return max(0, min(off, n-h))
}
