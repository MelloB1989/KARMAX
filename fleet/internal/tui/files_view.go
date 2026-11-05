package tui

import (
	"fmt"
	"image"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// The Files tab's look: the desk's (desk_view.go). A head row names the
// side and its folder; the folder's entries, a column of their sizes and
// ages; beside them the file under the cursor, previewed, or the folder,
// listed. With this machine's side shown too, the two stand side by side.
// The last row is the transfers under way, or what the cursor is on.

// columns is the list's box and the preview's (w 0: none) in a body w wide.
func (t *filesTab) columns(w int) (list, prev box) {
	if t.dual {
		half := (w - 1) / 2
		return box{0, 0, half, 0}, box{half + 1, 0, w - half - 1, 0}
	}
	if !t.preview || w < 64 {
		return box{0, 0, w, 0}, box{}
	}
	lw := min(max(34, w*42/100), 72)
	return box{0, 0, lw, 0}, box{lw + 1, 0, w - lw - 1, 0}
}

func (t *filesTab) View(c *Ctx, w, h int) string {
	if t.view != nil {
		t.w, t.h = w, h
		t.fullW, t.fullH = w, h
		return t.view.draw(t, c, w, h)
	}
	t.w, t.h = w, h
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
	list, prev := t.columns(w)
	bodyH := h - 1
	sep := lipgloss.NewStyle().Foreground(pal.line).Render("│")
	if t.dual {
		t.drawPane(c, put, t.panes[sideHost], box{list.x, 0, list.w, bodyH}, t.side == sideHost)
		for y := 0; y < bodyH; y++ {
			put(list.w, y, 1, sep)
		}
		t.drawPane(c, put, t.panes[sideHere], box{prev.x, 0, prev.w, bodyH}, t.side == sideHere)
	} else {
		t.drawPane(c, put, t.panes[sideHost], box{0, 0, list.w, bodyH}, true)
		if prev.w > 0 {
			for y := 0; y < bodyH; y++ {
				put(list.w, y, 1, sep)
			}
			t.drawPreview(c, put, box{prev.x, 0, prev.w, bodyH})
		}
	}
	put(0, h-1, w, t.status(c, w))
	if card := t.card(c, w); card != nil {
		cw := 0
		for _, l := range card {
			cw = max(cw, lipgloss.Width(l))
		}
		x, y := max(0, (w-cw)/2), max(0, (h-len(card))/3)
		for i, l := range card {
			put(x, y+i, cw, l)
		}
	}
	return buf.Render()
}

// card is the question asked, if one is.
func (t *filesTab) card(c *Ctx, w int) []string {
	th := c.Theme
	cw := min(w-4, 72)
	switch {
	case t.ask != nil:
		return t.ask.view(th, cw)
	case t.confirm != nil:
		return confirmCard(th, t.confirm, cw)
	case t.panel != nil:
		return t.panel.view(t, c, min(w-4, 100), t.h-2)
	}
	return nil
}

// drawPane is a side: its head, then its entries.
func (t *filesTab) drawPane(c *Ctx, put func(x, y, w int, s string), pn *fpane, b box, on bool) {
	th := c.Theme
	pal := paletteOf(th)
	// The head: the side, its folder, and how it is listed.
	name := t.sideName(pn.side)
	mark := seg{s: " ◆ ", fg: th.Amber}
	if pn.side == sideHere {
		mark = seg{s: " ◇ ", fg: th.Cyan}
	}
	if !on {
		mark.fg = th.Dim
	}
	var flags []string
	if pn.side == sideHost {
		if r := t.route(c); r != "" {
			flags = append(flags, r)
		}
	}
	if t.sortBy != sortName {
		flags = append(flags, "by "+t.sortBy)
	}
	if t.hidden {
		flags = append(flags, "hidden shown")
	}
	right := ""
	if len(flags) > 0 {
		right = th.Muted.Render(strings.Join(flags, " · ")) + " "
	}
	rw := lipgloss.Width(right)
	headBg := pal.head
	if on && t.dual {
		headBg = pal.tabOn
	}
	put(b.x, b.y, b.w, line(b.w-rw, headBg, mark, seg{s: name, fg: th.Text, bold: on}, seg{s: "  " + t.shownPath(pn), fg: th.Dim})+
		lipgloss.NewStyle().Background(headBg).Render(right))
	lb := box{b.x, b.y + 1, b.w, b.h - 1}
	switch {
	case pn.err != "" && pn.dir == nil:
		put(lb.x+2, lb.y+1, lb.w-2, th.Danger.Render("✕ ")+th.Muted.Render(pn.err))
		put(lb.x+2, lb.y+2, lb.w-2, th.Muted.Render("← goes up · g goes to a folder · R tries again"))
		return
	case pn.dir == nil:
		put(lb.x+2, lb.y+1, lb.w-2, th.Muted.Render("listing…"))
		return
	case len(pn.rows) == 0:
		msg := "empty"
		if f := strings.TrimSuffix(pn.filter, "\x00"); f != "" {
			msg = "nothing here has " + fmt.Sprintf("%q", f)
		} else if len(pn.dir.Entries) > 0 {
			msg = fmt.Sprintf("%d hidden: . shows them", len(pn.dir.Entries))
		}
		put(lb.x+2, lb.y+1, lb.w-2, th.Muted.Render(msg))
		return
	}
	pn.top = scrollTo(pn.top, pn.cur, len(pn.rows), lb.h)
	for i := 0; i < lb.h && pn.top+i < len(pn.rows); i++ {
		e := pn.rows[pn.top+i]
		put(lb.x, lb.y+i, lb.w, t.row(c, pn, e, pn.top+i == pn.cur, on, lb.w))
	}
	if pn.dir.More > 0 && lb.h > 2 {
		put(lb.x+2, lb.y+lb.h-1, lb.w-2, th.Muted.Render(fmt.Sprintf("… and %d more not listed", pn.dir.More)))
	}
}

// row is one entry: its mark, kind, name, size and age, in w cells.
func (t *filesTab) row(c *Ctx, pn *fpane, e fleetapi.FileEntry, cur, on bool, w int) string {
	th := c.Theme
	pal := paletteOf(th)
	var bg = pal.bar
	switch {
	case cur && on:
		bg = pal.rowSel
	case cur:
		bg = pal.tab
	default:
		bg = nil
	}
	icon, fg := "  ", th.Text
	switch {
	case e.IsDir():
		icon, fg = "▸ ", th.Frost
	case e.Kind == fleetapi.KindLink && e.To == "":
		icon, fg = "↯ ", th.Alert
	case e.Kind == fleetapi.KindOther:
		icon, fg = "◦ ", th.Dim
	case pictured(e.Name):
		icon, fg = "▣ ", th.Violet
	case strings.HasPrefix(e.Mode, "-") && strings.ContainsAny(e.Mode[1:], "x"):
		icon, fg = "▪ ", th.Green
	}
	lead := seg{s: " ", fg: th.Text}
	if pn.marks[e.Name] {
		lead = seg{s: "●", fg: th.Amber}
	}
	name := e.Name
	if e.IsDir() {
		name += "/"
	}
	if e.Kind == fleetapi.KindLink {
		name += " → " + e.Link
	}
	size := ""
	if !e.IsDir() && e.Kind != fleetapi.KindOther {
		size = bytesOf(e.Size)
	}
	age := ""
	if !e.Mod.IsZero() {
		age = ageShort(time.Since(e.Mod))
	}
	meta := fmt.Sprintf(" %8s %4s ", size, age)
	if w < 40 {
		meta = fmt.Sprintf(" %4s ", age)
	}
	mw := lipgloss.Width(meta)
	st := lipgloss.NewStyle().Foreground(th.Dim)
	if bg != nil {
		st = st.Background(bg)
	}
	return line(w-mw, bg, lead, seg{s: icon, fg: fg}, seg{s: name, fg: fg, bold: cur && on && e.IsDir()}) + st.Render(meta)
}

// ageShort is an age in one unit, four cells at most: 40s, 5m, 3h, 28d, 2y.
func ageShort(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 2*365*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return fmt.Sprintf("%dy", int(d.Hours()/24/365))
}

// drawPreview is what the cursor is on, beside the list.
func (t *filesTab) drawPreview(c *Ctx, put func(x, y, w int, s string), b box) {
	th := c.Theme
	pn := t.panes[sideHost]
	e, ok := pn.entry()
	if !ok {
		return
	}
	full := pn.join(e.Name)
	in := box{b.x + 2, b.y, b.w - 3, b.h}
	// What it is, in a line.
	what := e.Mode
	if e.Kind != fleetapi.KindDir {
		what += "  " + bytesOf(e.Size)
	}
	if !e.Mod.IsZero() {
		what += "  " + e.Mod.Local().Format("2 Jan 2006 15:04")
	}
	put(in.x, in.y, in.w, line(in.w, nil, seg{s: e.Name, fg: th.Text, bold: true}))
	put(in.x, in.y+1, in.w, th.Muted.Render(what))
	area := box{in.x, in.y + 3, in.w, in.h - 3}
	if e.IsDir() {
		d := t.lists[listKey(sideHost, full)]
		if d == nil {
			put(area.x, area.y, area.w, th.Muted.Render("…"))
			return
		}
		pv := t.peek(d)
		if len(pv.rows) == 0 {
			put(area.x, area.y, area.w, th.Muted.Render("empty"))
		}
		for i := 0; i < area.h && i < len(pv.rows); i++ {
			put(area.x, area.y+i, area.w, t.row(c, pv, pv.rows[i], false, false, area.w))
		}
		return
	}
	if e.Kind == fleetapi.KindLink && e.To == "" {
		put(area.x, area.y, area.w, th.Muted.Render("points at "+e.Link+", which is not there"))
		return
	}
	p := t.prevs[prevKey(sideHost, full, e)]
	switch {
	case p == nil:
		put(area.x, area.y, area.w, th.Muted.Render("…"))
	case p.kind == "err":
		put(area.x, area.y, area.w, th.Muted.Render(p.err))
	case p.kind == showNone:
		put(area.x, area.y, area.w, th.Muted.Render("not a file to read"))
	case p.kind == showImage:
		rows := t.picRows(previewPicKey(full, e), p.img, area.w, area.h, th)
		for i, r := range rows {
			put(area.x, area.y+i, area.w, r)
		}
	case p.kind == showMarkdown:
		if p.mdW != area.w {
			p.lines, p.mdW = markdown(p.text, area.w, th.Dark), area.w
		}
		for i := 0; i < area.h && i < len(p.lines); i++ {
			put(area.x, area.y+i, area.w, p.lines[i])
		}
	default:
		for i := 0; i < area.h && i < len(p.lines); i++ {
			put(area.x, area.y+i, area.w, p.lines[i])
		}
	}
}

// peek is a folder as the preview lists it, sorted once per listing.
func (t *filesTab) peek(d *fleetapi.FileDir) *fpane {
	k := fmt.Sprintf("%p:%s:%v", d, t.sortBy, t.hidden)
	if t.peeked != nil && t.peekKey == k {
		return t.peeked
	}
	pv := &fpane{side: sideHost, dir: d, marks: map[string]bool{}}
	t.relist(pv)
	t.peeked, t.peekKey = pv, k
	return pv
}

func previewPicKey(full string, e fleetapi.FileEntry) string {
	return "p:" + full + "@" + e.Mod.String()
}

// picRows is a picture drawn in at most cols×rows cells: kitty's
// placeholders once it is sent, or half blocks; nil while it is on its way.
func (t *filesTab) picRows(key string, img image.Image, cols, rows int, th *Theme) []string {
	if img == nil || cols <= 0 || rows <= 0 {
		return nil
	}
	b := img.Bounds()
	switch t.gfx {
	case gfxOff:
		return []string{th.Muted.Render(fmt.Sprintf("a picture, %d×%d: o opens it here", b.Dx(), b.Dy()))}
	case gfxKitty:
		c, r := fitCells(b.Dx(), b.Dy(), cols, rows, t.cw, t.ch)
		p := t.gal.lookup(key, c, r)
		if p == nil {
			return nil
		}
		return kittyRows(p.id, c, r)
	}
	c, r := blockCells(b.Dx(), b.Dy(), cols, rows, t.cw, t.ch)
	k := fmt.Sprintf("%s@%dx%d", key, c, r)
	if out := t.blocks[k]; out != nil {
		return out
	}
	if len(t.blocks) > 24 {
		clear(t.blocks)
	}
	out := halfBlocks(img, c, r, th.Ground)
	t.blocks[k] = out
	return out
}

// placePics sends the picture on screen to a kitty terminal, when it is
// new: in Update, before the view draws its placeholders.
func (t *filesTab) placePics() tea.Cmd {
	if t.gfx != gfxKitty || t.w <= 0 {
		return nil
	}
	key, img, cols, rows := t.shownPic()
	if img == nil {
		return nil
	}
	b := img.Bounds()
	c, r := fitCells(b.Dx(), b.Dy(), cols, rows, t.cw, t.ch)
	if c <= 0 || r <= 0 {
		return nil
	}
	_, cmd := t.gal.kittyPic(key, img, c, r)
	return cmd
}

// shownPic is the picture the view draws now, and the cells it may take.
func (t *filesTab) shownPic() (string, image.Image, int, int) {
	if v := t.view; v != nil {
		if v.as != showImage || v.img == nil {
			return "", nil, 0, 0
		}
		return v.picKey(), v.img, t.winW(), t.winH() - 2
	}
	_, prev := t.columns(t.w)
	if t.dual || prev.w <= 0 {
		return "", nil, 0, 0
	}
	pn := t.panes[sideHost]
	e, ok := pn.entry()
	if !ok || e.IsDir() {
		return "", nil, 0, 0
	}
	full := pn.join(e.Name)
	p := t.prevs[prevKey(sideHost, full, e)]
	if p == nil || p.kind != showImage || p.img == nil {
		return "", nil, 0, 0
	}
	// drawPreview's area: inside its margins, under the name and its line.
	return previewPicKey(full, e), p.img, prev.w - 3, t.h - 1 - 3
}

// route is how bytes go to fleetd's side, as the head shows it.
func (t *filesTab) route(c *Ctx) string {
	switch c.Client.FilesRoute() {
	case fleetapi.RouteLAN:
		return "⇄ local network"
	case fleetapi.RouteLocal:
		return "" // the same machine: no route to speak of
	}
	return "⇄ tailnet"
}

// status is the last row: a filter typed, the transfers, the marks, or
// what the cursor is on.
func (t *filesTab) status(c *Ctx, w int) string {
	th := c.Theme
	pn := t.pane()
	if t.filtering() {
		return line(w, nil, seg{s: " filter: ", fg: th.Amber}, seg{s: strings.TrimSuffix(pn.filter, "\x00") + "▏", fg: th.Text},
			seg{s: "   ⏎ keep · esc clear", fg: th.Dim})
	}
	if s := t.xferLine(c, w); s != "" {
		return s
	}
	var segs []seg
	if n := len(pn.marks); n > 0 {
		segs = append(segs, seg{s: fmt.Sprintf(" ● %d marked ", n), fg: th.Amber})
	}
	if f := pn.filter; f != "" {
		segs = append(segs, seg{s: fmt.Sprintf(" filter %q ", f), fg: th.Amber})
	}
	if pn.dir != nil {
		n := len(pn.rows)
		segs = append(segs, seg{s: fmt.Sprintf(" %d items", n), fg: th.Dim})
	}
	segs = append(segs, seg{s: "  ", fg: th.Dim})
	hint := "d download · u upload · o open here · T transfers"
	if t.dual {
		hint = "c copies to the other side · tab switches · T transfers"
	}
	segs = append(segs, seg{s: hint, fg: th.Dim})
	return line(w, nil, segs...)
}

// xferLine is the transfers under way, in a line; "" when none are.
func (t *filesTab) xferLine(c *Ctx, w int) string {
	th := c.Theme
	var up, down, n int
	var total, done int64
	var rate float64
	route, note := "", ""
	var last *xfer
	for _, x := range t.xf.all() {
		s := x.state.Load()
		if s != xferQueued && s != xferRunning {
			continue
		}
		n++
		if x.up {
			up++
		} else {
			down++
		}
		if s == xferRunning {
			last = x
			x.mu.Lock()
			rate += x.rate
			route = x.route
			if x.note != "" {
				note = x.note
			}
			x.mu.Unlock()
		}
		if tt := x.total.Load(); tt > 0 {
			total += tt
			done += min(x.done.Load(), tt)
		}
	}
	if n == 0 {
		return ""
	}
	var segs []seg
	dir := ""
	if down > 0 {
		dir += fmt.Sprintf("↓%d ", down)
	}
	if up > 0 {
		dir += fmt.Sprintf("↑%d ", up)
	}
	segs = append(segs, seg{s: " " + dir, fg: th.Cyan, bold: true})
	if last != nil {
		segs = append(segs, seg{s: last.name + " ", fg: th.Text})
	}
	if total > 0 {
		pct := float64(done) / float64(total)
		segs = append(segs, seg{s: bar(pct, 12) + fmt.Sprintf(" %d%% ", int(pct*100)), fg: th.Amber})
	}
	if rate > 0 {
		segs = append(segs, seg{s: bytesOf(int64(rate)) + "/s ", fg: th.Text})
	}
	switch route {
	case fleetapi.RouteLAN:
		segs = append(segs, seg{s: "local network ", fg: th.Green})
	case fleetapi.RouteTailnet:
		segs = append(segs, seg{s: "tailnet ", fg: th.Dim})
	}
	if note != "" {
		segs = append(segs, seg{s: "· " + note + " ", fg: th.Amber})
	}
	segs = append(segs, seg{s: "· T for all", fg: th.Dim})
	return line(w, nil, segs...)
}

// bar is a progress bar n cells wide.
func bar(p float64, n int) string {
	p = min(max(p, 0), 1)
	full := int(p * float64(n))
	return strings.Repeat("━", full) + strings.Repeat("─", n-full)
}

// xferPanel lists the transfers: x stops one, c clears the ended, o opens
// where one went.
type xferPanel struct{ hi int }

func (p *xferPanel) key(t *filesTab, k string, c *Ctx) tea.Cmd {
	xs := t.xf.all()
	switch k {
	case "esc", "T", "q":
		t.panel = nil
	case "up", "k":
		p.hi = max(0, p.hi-1)
	case "down", "j":
		p.hi = min(len(xs)-1, p.hi+1)
	case "x":
		if p.hi < len(xs) {
			x := xs[len(xs)-1-p.hi]
			x.mu.Lock()
			cancel := x.cancel
			x.mu.Unlock()
			if x.state.Load() == xferQueued {
				x.state.Store(xferCancelled)
			}
			if cancel != nil {
				cancel()
			}
		}
	case "c":
		t.xf.clear()
		p.hi = 0
	case "o", "enter":
		if p.hi < len(xs) {
			x := xs[len(xs)-1-p.hi]
			x.mu.Lock()
			out := x.out
			x.mu.Unlock()
			if x.state.Load() == xferDone && !x.up {
				openHere(out)
			}
		}
	}
	return nil
}

func (p *xferPanel) view(t *filesTab, c *Ctx, w, maxH int) []string {
	th := c.Theme
	xs := t.xf.all()
	lines := []string{""}
	if len(xs) == 0 {
		lines = append(lines, th.Muted.Render("No transfers yet: d downloads, u uploads, or drop files on the window."))
	}
	in := w - 6
	for i := len(xs) - 1; i >= 0 && len(lines) < maxH-6; i-- {
		x := xs[i]
		hi := len(xs)-1-i == p.hi
		x.mu.Lock()
		route, errS, out, note, rate := x.route, "", x.out, x.note, x.rate
		if x.err != nil {
			errS = oneLine(x.err.Error())
		}
		x.mu.Unlock()
		arrow := "↓"
		if x.up {
			arrow = "↑"
		}
		var state string
		var col = th.Dim
		done, total := x.done.Load(), x.total.Load()
		switch x.state.Load() {
		case xferQueued:
			state = "waiting"
		case xferRunning:
			col = th.Amber
			if total > 0 {
				state = fmt.Sprintf("%s %d%% %s of %s", bar(float64(done)/float64(total), 10), int(float64(done)*100/float64(total)), bytesOf(done), bytesOf(total))
			} else {
				state = bytesOf(done)
			}
			if rate > 0 {
				state += " · " + bytesOf(int64(rate)) + "/s"
			}
		case xferDone:
			col, state = th.Green, "done · "+bytesOf(max(done, total))
		case xferFailed:
			col, state = th.Alert, "failed"
		case xferCancelled:
			state = "stopped"
		}
		if route != "" {
			state += " · " + map[string]string{fleetapi.RouteLAN: "local network", fleetapi.RouteTailnet: "tailnet", fleetapi.RouteLocal: "this machine"}[route]
		}
		mark := "  "
		if hi {
			mark = th.Selected.Render("▸ ")
		}
		lines = append(lines, mark+line(in-2, nil, seg{s: arrow + " ", fg: th.Cyan, bold: true}, seg{s: x.name, fg: th.Text, bold: hi},
			seg{s: "  " + state, fg: col}))
		sub := ""
		switch {
		case errS != "":
			sub = errS
		case note != "":
			sub = note
		case out != "":
			sub = "→ " + t.tildeHere(out)
		default:
			sub = "→ " + x.dst
		}
		lines = append(lines, "    "+line(in-4, nil, seg{s: sub, fg: th.Dim}))
	}
	lines = append(lines, "", hintsFit(th, in, "↑↓ choose", "x stop", "o open", "c clear the ended", "esc close"))
	return deskCard(th.Cyan, th.Title.Render("Transfers"), "", lines, w)
}
