package tui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// The desk's look: cmux's, in cells. The shown workspace is a rounded sand
// card (its corners are quarter blocks), the rest plain; the sessions are
// a bar of tabs, or panes with a quiet head each; text that does not fit
// ends in an ellipsis, a hint that does not fit is left out whole, and a
// question wraps.

// deskPalette is the desk's colors for a dark or light terminal.
type deskPalette struct {
	sand, sandSel, sandText, sandMuted, sandAccent color.Color
	rowSel, bar, tab, tabOn, head, line            color.Color
}

func paletteOf(th *Theme) deskPalette {
	if th.Dark {
		return deskPalette{
			sand: lipgloss.Color("#E8D7A3"), sandSel: lipgloss.Color("#D5BF83"), sandText: lipgloss.Color("#1A1A1A"),
			sandMuted: lipgloss.Color("#6F6243"), sandAccent: lipgloss.Color("#9A5200"),
			rowSel: lipgloss.Color("#1E1E1E"), bar: lipgloss.Color("#101010"), tab: lipgloss.Color("#171717"),
			tabOn: lipgloss.Color("#2A2A2A"), head: lipgloss.Color("#141414"), line: lipgloss.Color("#2C2C2C"),
		}
	}
	return deskPalette{
		sand: lipgloss.Color("#F3E6BF"), sandSel: lipgloss.Color("#E6D49D"), sandText: lipgloss.Color("#1A1A1A"),
		sandMuted: lipgloss.Color("#776A48"), sandAccent: lipgloss.Color("#9A5200"),
		rowSel: lipgloss.Color("#ECECEC"), bar: lipgloss.Color("#F4F4F4"), tab: lipgloss.Color("#EAEAEA"),
		tabOn: lipgloss.Color("#FFFFFF"), head: lipgloss.Color("#F0F0F0"), line: lipgloss.Color("#D6D6D6"),
	}
}

// seg is a run of plain text in one style.
type seg struct {
	s      string
	fg     color.Color
	bold   bool
	italic bool
}

// line is segs on bg (nil: the terminal's) in exactly w cells: cut with an
// ellipsis where they do not fit, padded where they fall short.
func line(w int, bg color.Color, segs ...seg) string {
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	for _, sg := range segs {
		st := lipgloss.NewStyle().Foreground(sg.fg).Bold(sg.bold).Italic(sg.italic)
		if bg != nil {
			st = st.Background(bg)
		}
		tw := lipgloss.Width(sg.s)
		if used+tw > w {
			b.WriteString(st.Render(ansi.Truncate(sg.s, w-used, "…")))
			used = w
			break
		}
		b.WriteString(st.Render(sg.s))
		used += tw
	}
	if used < w {
		pad := lipgloss.NewStyle()
		if bg != nil {
			pad = pad.Background(bg)
		}
		b.WriteString(pad.Render(strings.Repeat(" ", w-used)))
	}
	return b.String()
}

// hintsFit is as many whole hints as fit in w, from the first: one that
// does not fit is left out, never cut.
func hintsFit(th *Theme, w int, items ...string) string {
	sep := th.Muted.Render(" · ")
	out, used := "", 0
	for _, it := range items {
		iw := lipgloss.Width(it)
		add := iw
		if out != "" {
			add += 3
		}
		if used+add > w {
			continue
		}
		if out != "" {
			out += sep
		}
		out += th.Muted.Render(it)
		used += add
	}
	return out
}

// hintRows is the hints in as many rows of w as they take, at most n.
func hintRows(th *Theme, w, n int, items ...string) []string {
	var rows []string
	for len(items) > 0 && len(rows) < n {
		used, k := 0, 0
		for k < len(items) {
			add := lipgloss.Width(items[k])
			if k > 0 {
				add += 3
			}
			if used+add > w {
				break
			}
			used += add
			k++
		}
		if k == 0 {
			k = 1 // one too wide: it alone, cut
		}
		rows = append(rows, hintsFit(th, w, items[:k]...))
		items = items[k:]
	}
	return rows
}

func statusGlyph(s fleetapi.DeskSession, frame int) string {
	switch {
	case !s.Running:
		return "✕"
	case s.Note != nil && s.Note.Kind == fleetapi.NoteNeeds, s.Status == fleetapi.StatusWaiting:
		return "◆"
	case s.Status == fleetapi.StatusWorking:
		return deskSpinner[frame%len(deskSpinner)]
	case isClaude(s.Command):
		return "○"
	}
	return "›"
}

var deskSpinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// needs is what a session's Claude Code asks of the operator, "" for
// nothing.
func needs(s fleetapi.DeskSession) string {
	switch {
	case s.Note != nil && s.Note.Kind == fleetapi.NoteNeeds:
		return s.Note.Text
	case s.Status == fleetapi.StatusWaiting:
		return "needs you"
	}
	return ""
}

func (d *deskTab) View(c *Ctx, w, h int) string {
	d.w, d.h, d.viewedAt = w, h, c.Now
	if w <= 0 || h <= 0 {
		return ""
	}
	th := c.Theme
	pal := paletteOf(th)
	buf := uv.NewScreenBuffer(w, h)
	put := func(x, y, width int, s string) {
		if width <= 0 || y < 0 || y >= h || x >= w {
			return
		}
		width = min(width, w-x)
		uv.NewStyledString(Fit(s, width)).Draw(buf, uv.Rect(x, y, width, 1))
	}
	d.hits = d.hits[:0]
	side, headY, area, footY := d.frame()
	if side.w > 0 {
		d.drawSidebar(c, put, side)
		sep := lipgloss.NewStyle().Foreground(pal.line).Render("│")
		for y := 0; y < h; y++ {
			put(side.w, y, 1, sep)
		}
	}
	if d.list == nil {
		msg := th.Muted.Render("connecting to fleetd…")
		if d.err != "" {
			msg = th.Danger.Render("✕ ") + th.Muted.Render(d.err)
		}
		put(area.x+2, area.y+1, area.w-2, msg)
		put(area.x, footY, area.w, d.footer(c, area.w))
		return buf.Render()
	}
	d.drawHead(c, put, box{area.x, headY, area.w, 1})
	ss, bs, bar := d.panes()
	if bar {
		d.drawTabs(c, put, box{area.x, area.y, area.w, 1})
	}
	sp := split(bs)
	if len(ss) == 0 {
		mid := area.y + area.h/2 - 1
		put(area.x, mid, area.w, centerLine(th.Strong.Render(d.wsName(d.active)+" is empty"), area.w))
		put(area.x, mid+1, area.w, centerLine(th.Muted.Render("n starts a session here · W makes another workspace"), area.w))
	}
	sep := lipgloss.NewStyle().Foreground(pal.line).Render("│")
	for i, s := range ss {
		b := bs[i]
		if b.w == 0 {
			continue
		}
		tb := termBox(b, sp)
		if sp {
			d.drawPaneHead(c, put, s, box{b.x, b.y, b.w, 1})
		}
		d.hits = append(d.hits, hit{b: b, session: s.Name, pane: true, term: tb})
		d.drawTerm(c, buf, put, s, tb)
		if sp && b.x+b.w < area.x+area.w {
			for y := b.y; y < b.y+b.h; y++ {
				put(b.x+b.w, y, 1, sep)
			}
		}
	}
	put(area.x, footY, area.w, d.footer(c, area.w))
	if card := d.card(c, area.w); card != nil {
		cw := 0
		for _, l := range card {
			cw = max(cw, lipgloss.Width(l))
		}
		x, y := area.x+max(0, (area.w-cw)/2), area.y+max(0, (area.h-len(card))/3)
		for i, l := range card {
			put(x, y+i, cw, l)
		}
	}
	return buf.Render()
}

func centerLine(s string, w int) string {
	return strings.Repeat(" ", max(0, (w-lipgloss.Width(s))/2)) + s
}

// drawSidebar is the workspaces: the shown one a rounded sand card with its
// sessions in it (and what any asks), the rest plain, two rows of hints at
// the foot.
func (d *deskTab) drawSidebar(c *Ctx, put func(x, y, w int, s string), side box) {
	th := c.Theme
	pal := paletteOf(th)
	x, w := side.x, side.w
	cw := w - 2 // a card is one cell in from each side
	brand := line(15, nil, seg{s: " "}, seg{s: "KARMAX", fg: th.Amber, bold: true}, seg{s: " ▸ desk", fg: th.Dim})
	host := ""
	if d.list != nil && d.list.Host != "" && w > 18 {
		host = ansi.Truncate(d.list.Host, w-17, "…")
	}
	put(x, 0, w, brand+strings.Repeat(" ", max(0, w-16-lipgloss.Width(host)))+th.Muted.Render(host))
	hints := []string{"⏎ type", "^] out", "n new", "W workspace", "l layout", "r rename", "m move", "x end", "u news", "! notices"}
	if d.mode == typeMode {
		hints = []string{"^] step out", "^V paste file/image", "⌘B sidebar", "⌘U news"}
	}
	foot := hintRows(th, w-1, 2, hints...)
	limit := side.h - len(foot) - 1
	y := 2
	rows := d.rows()
	ri := 0
	for _, ws := range d.workspaces() {
		ss := d.in(ws.ID)
		shown := ws.ID == d.active
		bg, text, muted, accent := color.Color(nil), th.Text, th.Dim, th.Amber
		if shown {
			bg, text, muted, accent = pal.sand, pal.sandText, pal.sandMuted, pal.sandAccent
		}
		var body []string
		var bodyHits []hit
		row := func(rbg color.Color, segs ...seg) {
			if rbg == nil {
				rbg = bg
			}
			body = append(body, line(cw, rbg, append([]seg{{s: " "}}, segs...)...))
		}
		// the name, and the cursor on it
		cur := d.mode == navMode && ri < len(rows) && d.cursor == ri && rows[ri].session == ""
		var nameBg color.Color
		mark := " "
		if cur {
			mark = "▸"
			if !shown {
				nameBg = pal.rowSel
			}
		}
		count := ""
		if n := len(ss); n > 0 {
			count = fmt.Sprintf("  %d", n)
		}
		row(nameBg, seg{s: mark, fg: accent}, seg{s: ws.Name, fg: text, bold: true}, seg{s: count, fg: muted})
		row(nil, d.wsStatus(c, ss, text, muted, accent)...)
		if dirs := wsDirs(ss); dirs != "" {
			row(nil, seg{s: dirs, fg: muted})
		}
		ri++
		if shown {
			for _, s := range ss {
				cur := d.mode == navMode && d.cursor == ri
				rbg := bg
				if cur {
					rbg = pal.sandSel
				}
				mark := "  "
				if cur {
					mark = "▸ "
				}
				segs := []seg{{s: mark, fg: accent}, {s: statusGlyph(s, c.Frame) + " ", fg: glyphColor(th, pal, s, true)},
					{s: shownName(s), fg: text, bold: s.Name == d.focused()}}
				if u := d.unread[s.Name]; u != "" {
					segs = append(segs, seg{s: " " + dotOf(u), fg: accent, bold: true})
				}
				row(rbg, segs...)
				bodyHits = append(bodyHits, hit{b: box{x + 1, 0, cw, 1}, session: s.Name}) // y set below
				if n := needs(s); n != "" {
					row(nil, seg{s: "    " + n, fg: accent, italic: true})
					bodyHits = append(bodyHits, hit{b: box{x + 1, 0, cw, 1}, session: s.Name})
				}
				ri++
			}
		}
		need := len(body)
		if shown {
			need += 2 // its edges
		}
		if y+need > limit {
			if y < limit {
				put(x+1, y, cw, th.Muted.Render(fmt.Sprintf(" +%d more · { } or alt+1–9", len(d.workspaces())-wsIndex(d.workspaces(), ws.ID))))
			}
			break
		}
		start := y
		if shown {
			edge := lipgloss.NewStyle().Foreground(pal.sand)
			put(x+1, y, cw, edge.Render("▗"+strings.Repeat("▄", max(0, cw-2))+"▖"))
			y++
		}
		// rows: the name, status, dirs, then sessions (whose hits come in order)
		hi := 0
		for i, l := range body {
			put(x+1, y, cw, l)
			if shown && i >= len(body)-len(bodyHits) && hi < len(bodyHits) {
				h := bodyHits[hi]
				h.b.y = y
				d.hits = append(d.hits, h)
				hi++
			}
			y++
		}
		if shown {
			edge := lipgloss.NewStyle().Foreground(pal.sand)
			put(x+1, y, cw, edge.Render("▝"+strings.Repeat("▀", max(0, cw-2))+"▘"))
			y++
		}
		d.hits = append(d.hits, hit{b: box{x, start, w, y - start}, ws: ws.ID})
		y++ // a row between cards
	}
	for i, l := range foot {
		put(x+1, side.h-len(foot)+i, w-1, l)
	}
}

func wsIndex(wss []fleetapi.DeskWorkspace, id string) int {
	for i, w := range wss {
		if w.ID == id {
			return i
		}
	}
	return 0
}

func dotOf(why string) string {
	if why == fleetapi.NoteNeeds || why == "waiting" {
		return "◆"
	}
	return "●"
}

// glyphColor is a session's status glyph's color.
func glyphColor(th *Theme, pal deskPalette, s fleetapi.DeskSession, onSand bool) color.Color {
	switch {
	case !s.Running:
		return th.Alert
	case needs(s) != "", s.Status == fleetapi.StatusWorking:
		if onSand {
			return pal.sandAccent
		}
		return th.Amber
	}
	if onSand {
		return pal.sandMuted
	}
	return th.Dim
}

// wsStatus is a workspace's state: who needs the operator and for what,
// else how many work, else idle.
func (d *deskTab) wsStatus(c *Ctx, ss []fleetapi.DeskSession, text, muted, accent color.Color) []seg {
	var asking, done []fleetapi.DeskSession
	working := 0
	for _, s := range ss {
		if needs(s) != "" {
			asking = append(asking, s)
		} else if d.unread[s.Name] != "" {
			done = append(done, s)
		}
		if s.Status == fleetapi.StatusWorking {
			working++
		}
	}
	switch {
	case len(asking) == 1:
		return []seg{{s: "◆ ", fg: accent}, {s: shownName(asking[0]) + " " + needs(asking[0]), fg: accent, bold: true}}
	case len(asking) > 1:
		return []seg{{s: fmt.Sprintf("◆ %d need you", len(asking)), fg: accent, bold: true}}
	case len(done) == 1:
		return []seg{{s: "● ", fg: accent}, {s: shownName(done[0]) + " finished", fg: text, bold: true}}
	case len(done) > 1:
		return []seg{{s: fmt.Sprintf("● %d finished", len(done)), fg: text, bold: true}}
	case working > 0:
		return []seg{{s: deskSpinner[c.Frame%len(deskSpinner)] + fmt.Sprintf(" %d working", working), fg: accent}}
	case len(ss) == 0:
		return []seg{{s: "empty", fg: muted}}
	}
	return []seg{{s: "○ idle", fg: muted}}
}

// wsDirs is where a workspace's sessions are: the first directory and
// branch, and how many others.
func wsDirs(ss []fleetapi.DeskSession) string {
	var dirs []string
	branch := ""
	for _, s := range ss {
		dir := s.Cwd
		if dir == "" {
			dir = s.Dir
		}
		dir = shortDir(dir)
		if !contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
		if branch == "" {
			branch = s.Branch
		}
	}
	if len(dirs) == 0 {
		return ""
	}
	out := dirs[0]
	if branch != "" {
		out += " · " + branch
	}
	if len(dirs) > 1 {
		out += fmt.Sprintf(" +%d", len(dirs)-1)
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// drawHead is the top line: the workspace and where its focused session
// is, its layout and the mode.
func (d *deskTab) drawHead(c *Ctx, put func(x, y, w int, s string), b box) {
	th := c.Theme
	ws, _ := d.ws(d.active)
	mode := lipgloss.NewStyle().Foreground(th.Ground).Background(th.Green).Bold(true).Render(" TYPING ")
	if d.mode == navMode {
		mode = lipgloss.NewStyle().Foreground(th.Ground).Background(th.Amber).Bold(true).Render(" NAV ")
	}
	layout := th.Muted.Render(ws.Layout)
	if d.zoom {
		layout = paint(th.Amber, "zoomed")
	}
	if n := d.waitingCount(); n > 0 {
		layout = paint(th.Amber, fmt.Sprintf("◆ %d need you", n)) + th.Muted.Render(" · ") + layout
	}
	right := layout + " " + mode
	rw := lipgloss.Width(right)
	where := ""
	if s, ok := d.session(d.focused()); ok {
		where = shortDir(s.Cwd)
		if where == "" {
			where = shortDir(s.Dir)
		}
		if s.Branch != "" {
			where += " · " + s.Branch
		}
	}
	left := line(max(0, b.w-rw-1), nil, seg{s: " ▰ ", fg: th.Amber}, seg{s: ws.Name, fg: th.Text, bold: true}, seg{s: "   " + where, fg: th.Dim})
	put(b.x, b.y, b.w, left+" "+right)
	d.hits = append(d.hits, hit{b: box{b.x + b.w - rw, b.y, lipgloss.Width(layout), 1}, layout: true})
}

func (d *deskTab) waitingCount() int {
	n := 0
	for _, s := range d.sessions() {
		if needs(s) != "" {
			n++
		}
	}
	return n
}

// drawTabs is the shown workspace's sessions as tabs: each its status and
// name, the focused one lit; too many to fit, a window of them around the
// focused one, and how many more each side.
func (d *deskTab) drawTabs(c *Ctx, put func(x, y, w int, s string), b box) {
	th := c.Theme
	pal := paletteOf(th)
	ss := d.in(d.active)
	put(b.x, b.y, b.w, line(b.w, pal.bar))
	if len(ss) == 0 {
		return
	}
	const maxTab, minTab = 34, 14
	ws := make([]int, len(ss))
	label := func(s fleetapi.DeskSession) string { return statusGlyph(s, c.Frame) + " " + shownName(s) }
	total := 0
	for i, s := range ss {
		ws[i] = min(maxTab, lipgloss.Width(label(s))+4) // "▍ " … " •"
		total += ws[i] + 1
	}
	if total-1 > b.w { // share the width, no tab under minTab
		each := max(minTab, (b.w-(len(ss)-1))/len(ss))
		total = 0
		for i := range ws {
			ws[i] = min(ws[i], each)
			total += ws[i] + 1
		}
	}
	fi := 0
	for i, s := range ss {
		if s.Name == d.focused() {
			fi = i
		}
	}
	// the focused tab is read whole, however many share the bar
	ws[fi] = min(lipgloss.Width(label(ss[fi]))+4, max(maxTab, b.w*2/3), b.w)
	// the window: from lo to hi, holding the focused tab, as wide as fits
	lo, hi := 0, len(ss)
	fits := func(lo, hi int) bool {
		w := 0
		for i := lo; i < hi; i++ {
			w += ws[i] + 1
		}
		if lo > 0 {
			w += 4
		}
		if hi < len(ss) {
			w += 4
		}
		return w-1 <= b.w
	}
	for !fits(lo, hi) && hi-lo > 1 {
		if fi-lo > hi-1-fi {
			lo++
		} else {
			hi--
		}
	}
	x := b.x
	if lo > 0 {
		put(x, b.y, 4, line(4, pal.bar, seg{s: fmt.Sprintf(" ‹%d", lo), fg: th.Dim}))
		x += 4
	}
	for i := lo; i < hi; i++ {
		s := ss[i]
		on := s.Name == d.focused()
		bg, fg, bar := pal.tab, th.Dim, " "
		if on {
			bg, fg, bar = pal.tabOn, th.Text, "▍"
		}
		segs := []seg{{s: bar, fg: th.Amber}, {s: statusGlyph(s, c.Frame) + " ", fg: glyphColor(th, pal, s, false)}, {s: shownName(s), fg: fg, bold: on}}
		if u := d.unread[s.Name]; u != "" {
			// the dot must show however the name is cut
			nameW := ws[i] - 2 - 2 - 2
			segs[2].s = ansi.Truncate(segs[2].s, max(1, nameW), "…")
			segs = append(segs, seg{s: " " + dotOf(u), fg: th.Amber, bold: true})
		}
		put(x, b.y, ws[i], line(ws[i], bg, segs...))
		d.hits = append(d.hits, hit{b: box{x, b.y, ws[i], 1}, session: s.Name})
		x += ws[i] + 1
	}
	if hi < len(ss) {
		put(x, b.y, 4, line(4, pal.bar, seg{s: fmt.Sprintf("%d› ", len(ss)-hi), fg: th.Dim}))
	}
}

// drawPaneHead is a split pane's top line: its session and where it is; the
// focused one lit, sand while typing in it.
func (d *deskTab) drawPaneHead(c *Ctx, put func(x, y, w int, s string), s fleetapi.DeskSession, b box) {
	th := c.Theme
	pal := paletteOf(th)
	where := shortDir(s.Cwd)
	if where == "" {
		where = shortDir(s.Dir)
	}
	if s.Branch != "" {
		where += " · " + s.Branch
	}
	bg, fg, muted, onSand := pal.head, th.Dim, th.Dim, false
	if s.Name == d.focused() {
		bg, fg, muted = pal.tabOn, th.Text, th.Dim
		if d.mode == typeMode {
			bg, fg, muted, onSand = pal.sand, pal.sandText, pal.sandMuted, true
		}
	}
	segs := []seg{{s: " " + statusGlyph(s, c.Frame) + " ", fg: glyphColor(th, pal, s, onSand)}, {s: shownName(s), fg: fg, bold: s.Name == d.focused()}}
	if n := needs(s); n != "" {
		acc := th.Amber
		if onSand {
			acc = pal.sandAccent
		}
		segs = append(segs, seg{s: "  ◆ " + n, fg: acc, bold: true})
	} else if u := d.unread[s.Name]; u != "" {
		segs = append(segs, seg{s: " " + dotOf(u), fg: th.Amber, bold: true})
	}
	segs = append(segs, seg{s: "   " + where, fg: muted})
	put(b.x, b.y, b.w, line(b.w, bg, segs...))
}

// drawTerm is a session's terminal in b, and over it what keeps it from
// being live.
func (d *deskTab) drawTerm(c *Ctx, buf uv.ScreenBuffer, put func(x, y, w int, s string), s fleetapi.DeskSession, b box) {
	th := c.Theme
	pal := paletteOf(th)
	if b.w < 2 || b.h < 1 {
		return
	}
	if !s.Running {
		why := "not running"
		if s.Error != "" {
			why = s.Error
		}
		for i, l := range strings.Split(ansi.Wordwrap(why, max(10, b.w-6), ""), "\n") {
			if i == 0 {
				l = "✕ " + l
			} else {
				l = "  " + l
			}
			put(b.x+2, b.y+1+i, b.w-2, th.Muted.Render(l))
		}
		put(b.x+2, b.y+3+strings.Count(why, "\n"), b.w-2, th.Muted.Render("x ends it (Ctrl-] first)"))
		return
	}
	t := d.term(s.Name)
	if t == nil {
		put(b.x+2, b.y+1, b.w-2, th.Muted.Render("connecting…"))
		return
	}
	t.emu.Draw(buf, uv.Rect(b.x, b.y, b.w, b.h))
	st, holder, why := t.status()
	var segs []seg
	switch st {
	case termConnecting:
		segs = []seg{{s: "◌ connecting…", fg: th.Amber}}
	case termDown, termIdle:
		segs = []seg{{s: "◌ connecting…", fg: th.Amber}}
		if why != "" {
			segs = []seg{{s: "◌ reconnecting", fg: th.Amber, bold: true}, {s: " · ⏎ now · " + why, fg: th.Dim}}
		}
	case termHeld:
		segs = []seg{{s: holder + " types here", fg: th.Amber, bold: true}, {s: " · ⏎ takes the keyboard", fg: th.Dim}}
	case termReleased:
		segs = []seg{{s: "watching: " + holder + " took the keyboard", fg: th.Amber, bold: true}, {s: " · ⏎ takes it back", fg: th.Dim}}
	case termEnded:
		segs = []seg{{s: "✕ " + why, fg: th.Alert}}
	}
	if segs != nil {
		nw := 2
		for _, sg := range segs {
			nw += lipgloss.Width(sg.s)
		}
		nw = min(nw, b.w)
		put(b.x+b.w-nw, b.y, nw, line(nw, pal.tabOn, append([]seg{{s: " "}}, segs...)...))
	}
}

// footer is the bottom line: a toast while new, else what the keys do, as
// many as fit whole.
func (d *deskTab) footer(c *Ctx, w int) string {
	th := c.Theme
	if d.toast.Text != "" && c.Now.Sub(d.toastT) < 5*time.Second {
		if d.toast.Err {
			return line(w, nil, seg{s: " ✕ ", fg: th.Alert}, seg{s: oneLine(d.toast.Text), fg: th.Text})
		}
		return line(w, nil, seg{s: " ✓ ", fg: th.Green}, seg{s: oneLine(d.toast.Text), fg: th.Text})
	}
	if d.err != "" && d.list != nil {
		return line(w, nil, seg{s: " ▲ fleetd: " + d.err, fg: th.Alert}, seg{s: " · showing the last desk", fg: th.Dim})
	}
	if d.mode == typeMode {
		return " " + hintsFit(th, w-1, "typing in "+shownNameOf(d, d.focused()), "Ctrl-] step out", "Ctrl+V paste a file or image",
			"⌘B sidebar", "⌘1–9 workspace", "⌘U what needs you")
	}
	return " " + hintsFit(th, w-1, "↑↓ choose", "⏎ type", "u what needs you", "! notices", "n new", "W workspace", "l layout",
		"z zoom", "r rename", "m move", "x end", "b sidebar", "1–9 fleetctl's tabs", "? keys")
}

func shownNameOf(d *deskTab, name string) string {
	if s, ok := d.session(name); ok {
		return shownName(s)
	}
	return name
}

// card is the question asked, if one is: the launch form, a name, a
// workspace to move to, a yes or no, the notices.
func (d *deskTab) card(c *Ctx, w int) []string {
	th := c.Theme
	cw := min(w-4, 72)
	switch {
	case d.form != nil:
		return d.formView(c, w, d.h-2)
	case d.ask != nil:
		return d.ask.view(th, cw)
	case d.pick != nil:
		return d.pick.view(d, th, min(w-4, 56))
	case d.panel != nil:
		return d.panel.view(d, c, min(w-4, 96))
	case d.confirm != nil:
		return confirmCard(th, d.confirm, cw)
	}
	return nil
}

// wrapped is text in lines of w, each styled.
func wrapped(text string, w int, st lipgloss.Style) []string {
	var out []string
	for _, l := range strings.Split(ansi.Wordwrap(text, max(8, w), ""), "\n") {
		out = append(out, st.Render(l))
	}
	return out
}

// confirmCard asks a yes or no, the question whole, wrapped.
func confirmCard(th *Theme, cf *confirm, w int) []string {
	lines := []string{""}
	lines = append(lines, wrapped(cf.ask, w-6, lipgloss.NewStyle().Foreground(th.Text))...)
	lines = append(lines, "", hint(th, keyIs("y", "yes"))+th.Muted.Render(" · ")+hint(th, keyIs("n", "no")))
	return deskCard(th.Amber, th.Title.Render("Sure?"), "", lines, w)
}

// Cursor is where the terminal's own cursor goes: in the focused session,
// where its program put it, while typing in it.
func (d *deskTab) Cursor() *tea.Cursor {
	if d.mode != typeMode || d.Capturing() {
		return nil
	}
	name := d.focused()
	b, ok := d.termBoxes[name]
	t := d.term(name)
	if !ok || t == nil {
		return nil
	}
	if st, _, _ := t.status(); st != termLive || t.emu.CursorHidden() {
		return nil
	}
	p := t.emu.CursorPosition()
	if p.X >= b.w || p.Y >= b.h {
		return nil
	}
	cur := tea.NewCursor(b.x+p.X, b.y+p.Y)
	cur.Blink = false
	return cur
}

// WindowTitle says who needs the operator.
func (d *deskTab) WindowTitle() string {
	if n := d.waitingCount(); n > 0 {
		return fmt.Sprintf("◆ %d need you · fleetctl", n)
	}
	return ""
}

// noticePanel lists what Claude Code said: ⏎ goes to its session.
type noticePanel struct{ hi int }

func (p *noticePanel) key(d *deskTab, k string) tea.Cmd {
	switch k {
	case "esc", "q", "!":
		d.panel = nil
	case "up", "k":
		p.hi = max(0, p.hi-1)
	case "down", "j":
		p.hi = min(max(0, len(d.notices)-1), p.hi+1)
	case "c":
		d.notices, d.unread = nil, map[string]string{}
		d.panel = nil
		return toastCmd("notices cleared", false)
	case "enter":
		d.panel = nil
		if p.hi < len(d.notices) {
			d.goTo(d.notices[p.hi].session)
		}
	}
	return nil
}

func (p *noticePanel) view(d *deskTab, c *Ctx, w int) []string {
	th := c.Theme
	in := w - 6
	lines := []string{""}
	if len(d.notices) == 0 {
		lines = append(lines, th.Muted.Render("Nothing from Claude yet. A session that asks for you, or"),
			th.Muted.Render("finishes while you look elsewhere, shows up here."))
	}
	rows := min(len(d.notices), max(3, d.h-12))
	top := keepInView(0, p.hi, len(d.notices), rows)
	for i := top; i < top+rows && i < len(d.notices); i++ {
		n := d.notices[i]
		s, ok := d.session(n.session)
		name := n.session
		if ok {
			name = shownName(s)
		}
		fg, mark := th.Text, "  "
		if i == p.hi {
			fg, mark = th.Amber, "▸ "
		}
		kind := "●"
		if n.kind == fleetapi.NoteNeeds {
			kind = "◆"
		}
		lines = append(lines, line(in, nil, seg{s: mark, fg: th.Amber}, seg{s: Since(c.Now, n.at) + " ", fg: th.Dim},
			seg{s: kind + " ", fg: th.Amber}, seg{s: name, fg: fg, bold: i == p.hi}, seg{s: " · " + d.wsName(n.ws) + "  ", fg: th.Dim},
			seg{s: n.text, fg: fg}))
	}
	lines = append(lines, "", hintsFit(th, in, "⏎ go there", "↑↓ choose", "c clear", "esc close"))
	return deskCard(th.Amber, th.Title.Render("From Claude"), th.Muted.Render(fmt.Sprintf("%d", len(d.notices))), lines, w)
}
