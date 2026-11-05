package tui

import (
	"context"
	"fmt"
	"image"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/hostfiles"
)

// The viewer: one file, full screen. Text is read up to viewMax (a bigger
// file says so, and d fetches it whole), code in its colors with line
// numbers, markdown as a page (m shows its text), anything else as hex; a
// picture is asked for at the screen's size. [ and ] (← → for pictures)
// step through the folder's files.

const (
	viewMax = 4 << 20
	hexMax  = 256 << 10
)

type fviewer struct {
	side    int
	full    string
	e       fleetapi.FileEntry
	as      string
	raw     []byte
	more    bool
	lines   []string // highlighted text, or hex
	page    []string // markdown laid out at pageW
	pageW   int
	plain   bool // markdown as its text
	wrap    bool
	top     int
	left    int
	img     image.Image
	err     string
	loading bool
	sibs    []fleetapi.FileEntry
	at      int
	gen     int
	finding bool
	query   string
	hits    []int
	hit     int
	wrapped []string // the lines as wrapped at wrapW, and each one's number
	wrapNo  []int
	wrapW   int
}

type viewLoadedMsg struct {
	gen   int
	raw   []byte
	more  bool
	as    string
	lines []string
	page  []string
	pageW int
	img   image.Image
	err   error
}

// openView shows a file full screen.
func (t *filesTab) openView(pn *fpane, e fleetapi.FileEntry, c *Ctx) tea.Cmd {
	v := &fviewer{side: pn.side}
	for _, r := range pn.rows {
		if !r.IsDir() {
			if r.Name == e.Name {
				v.at = len(v.sibs)
			}
			v.sibs = append(v.sibs, r)
		}
	}
	t.view = v
	return v.show(t, pn, e, c)
}

// show loads entry e of the folder into the viewer.
func (v *fviewer) show(t *filesTab, pn *fpane, e fleetapi.FileEntry, c *Ctx) tea.Cmd {
	v.gen++
	v.e, v.full = e, pn.join(e.Name)
	v.as, v.raw, v.lines, v.page, v.img, v.err, v.loading = "", nil, nil, nil, nil, "", true
	v.top, v.left, v.hits, v.wrapped, v.wrapW, v.query = 0, 0, nil, nil, 0, ""
	gen, side, full, name := v.gen, v.side, v.full, e.Name
	cl, dark := c.Client, c.Theme.Dark
	w, h := t.winW(), t.winH()
	pw, ph := max(64, w*t.cw), max(64, (h-2)*t.ch)
	here := t.here
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out := viewLoadedMsg{gen: gen}
		if pictured(name) {
			var b []byte
			var err error
			if side == sideHost {
				b, err = cl.FilesPreview(ctx, full, pw, ph)
			} else {
				b, _, err = here.Preview(ctx, full, pw, ph)
			}
			if err == nil {
				out.img, err = decodePic(b)
			}
			out.as, out.err = showImage, err
			return out
		}
		raw, err := readHead(ctx, cl, side, full, viewMax+1)
		if err != nil {
			out.err = err
			return out
		}
		if len(raw) > viewMax {
			raw, out.more = raw[:viewMax], true
		}
		out.raw, out.as = raw, showAs(name, raw)
		switch out.as {
		case showHex:
			out.lines = hexLines(raw[:min(len(raw), hexMax)], 0)
		case showMarkdown:
			out.page, out.pageW = markdown(string(raw), w-2, dark), w
			out.lines = highlight(name, string(raw), dark)
		default:
			out.as = showText
			out.lines = highlight(name, string(raw), dark)
		}
		return out
	}
}

func (v *fviewer) loaded(m viewLoadedMsg, t *filesTab, c *Ctx) tea.Cmd {
	if m.gen != v.gen {
		return nil
	}
	v.loading = false
	if m.err != nil {
		v.err = oneLine(m.err.Error())
		return nil
	}
	v.raw, v.more, v.as, v.lines, v.page, v.pageW, v.img = m.raw, m.more, m.as, m.lines, m.page, m.pageW, m.img
	return nil
}

// winW and winH are the terminal's size, which the viewer fills.
func (t *filesTab) winW() int {
	if t.fullW > 0 {
		return t.fullW
	}
	return max(t.w+4, 40)
}

func (t *filesTab) winH() int {
	if t.fullH > 0 {
		return t.fullH
	}
	return max(t.h+frameRows, 12)
}

// shown is the viewer's lines as they are shown at width w: the page,
// or the text (wrapped when asked), with each line's number.
func (v *fviewer) shown(w int, dark bool) ([]string, []int) {
	if v.as == showMarkdown && !v.plain {
		if v.pageW != w && len(v.raw) > 0 {
			v.page, v.pageW = markdown(string(v.raw), w-2, dark), w
		}
		return v.page, nil
	}
	if !v.wrap || v.as == showHex {
		return v.lines, nil
	}
	tw := w - gutterW(len(v.lines)) - 1
	if v.wrapW != tw || v.wrapped == nil {
		v.wrapped, v.wrapNo, v.wrapW = nil, nil, tw
		for i, l := range v.lines {
			for k, part := range strings.Split(ansi.Hardwrap(l, max(8, tw), true), "\n") {
				v.wrapped = append(v.wrapped, part)
				if k == 0 {
					v.wrapNo = append(v.wrapNo, i+1)
				} else {
					v.wrapNo = append(v.wrapNo, 0)
				}
			}
		}
	}
	return v.wrapped, v.wrapNo
}

func gutterW(n int) int { return len(strconv.Itoa(max(n, 1))) + 1 }

func (v *fviewer) key(m tea.KeyPressMsg, t *filesTab, c *Ctx) tea.Cmd {
	k := m.String()
	if v.finding {
		switch k {
		case "esc":
			v.finding, v.query, v.hits = false, "", nil
		case "enter":
			v.finding = false
			v.find(t, c, 0)
		case "backspace":
			if r := []rune(v.query); len(r) > 0 {
				v.query = string(r[:len(r)-1])
			}
		default:
			if m.Text != "" {
				v.query += m.Text
			}
		}
		return nil
	}
	lines, _ := v.shown(t.winW(), c.Theme.Dark)
	page := max(1, t.winH()-3)
	maxTop := max(0, len(lines)-page)
	scroll := func(to int) { v.top = min(max(to, 0), maxTop) }
	step := func(by int) tea.Cmd {
		if len(v.sibs) == 0 {
			return nil
		}
		i := v.at + by
		if i < 0 || i >= len(v.sibs) {
			return nil
		}
		v.at = i
		return v.show(t, t.panes[v.side], v.sibs[i], c)
	}
	switch k {
	case "esc", "q", "backspace":
		t.view = nil
		pn := t.panes[v.side]
		for i, r := range pn.rows { // the cursor on the file shown last
			if r.Name == v.e.Name {
				pn.cur = i
			}
		}
		return t.rest()
	case "up", "k":
		scroll(v.top - 1)
	case "down", "j":
		scroll(v.top + 1)
	case "pgup", "b", "ctrl+b":
		scroll(v.top - page)
	case "pgdown", "space", "ctrl+f":
		scroll(v.top + page)
	case "home", "g":
		scroll(0)
		v.left = 0
	case "end", "G":
		scroll(maxTop)
	case "[":
		return step(-1)
	case "]":
		return step(1)
	case "left", "h":
		if v.as == showImage || v.wrap || v.as == showMarkdown && !v.plain {
			return step(-1)
		}
		v.left = max(0, v.left-8)
	case "right", "l":
		if v.as == showImage || v.wrap || v.as == showMarkdown && !v.plain {
			return step(1)
		}
		v.left += 8
	case "w":
		v.wrap, v.left, v.top = !v.wrap, 0, 0
	case "m":
		if v.as == showMarkdown {
			v.plain, v.top = !v.plain, 0
		}
	case "/":
		if v.as != showImage {
			v.finding, v.query = true, ""
		}
	case "n":
		v.find(t, c, 1)
	case "N":
		v.find(t, c, -1)
	case "d":
		if v.side == sideHost {
			return t.download([]fleetapi.FileEntry{v.e}, t.downDir, false, c)
		}
	case "o":
		if v.side == sideHere || t.local {
			openHere(v.full)
			return toast("opened "+v.e.Name, false)
		}
		return t.download([]fleetapi.FileEntry{v.e}, filepath.Join(openDir(), hostfiles.NewUploadID()[:8]), true, c)
	case "y":
		return tea.Batch(tea.SetClipboard(v.full), toast("copied "+v.full, false))
	}
	return nil
}

// find moves to the next line (by 1), the one before (-1), or the first
// from the top (0) that holds the query.
func (v *fviewer) find(t *filesTab, c *Ctx, by int) {
	if v.query == "" {
		return
	}
	lines, _ := v.shown(t.winW(), c.Theme.Dark)
	q := strings.ToLower(v.query)
	v.hits = v.hits[:0]
	for i, l := range lines {
		if strings.Contains(strings.ToLower(ansi.Strip(l)), q) {
			v.hits = append(v.hits, i)
		}
	}
	if len(v.hits) == 0 {
		return
	}
	switch by {
	case 0:
		v.hit = 0
		for i, h := range v.hits {
			if h >= v.top {
				v.hit = i
				break
			}
		}
	default:
		v.hit = (v.hit + by + len(v.hits)) % len(v.hits)
	}
	v.top = max(0, v.hits[v.hit]-3)
}

// draw is the viewer in w×h.
func (v *fviewer) draw(t *filesTab, c *Ctx, w, h int) string {
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
	// The head: where, what, how big.
	where := t.sideName(v.side)
	info := []string{bytesOf(v.e.Size), Age(time.Since(v.e.Mod)) + " ago"}
	if v.more {
		info = append(info, "the first "+bytesOf(viewMax))
	}
	if len(v.sibs) > 1 {
		info = append(info, fmt.Sprintf("%d of %d", v.at+1, len(v.sibs)))
	}
	right := th.Muted.Render(strings.Join(info, " · ") + " ")
	put(0, 0, w, line(w-lipgloss.Width(right), pal.head, seg{s: " ◆ ", fg: th.Amber}, seg{s: v.e.Name, fg: th.Text, bold: true},
		seg{s: "  " + where + ":" + t.shownDir(v.side, filepath.Dir(v.full)), fg: th.Dim})+right)
	body := box{0, 1, w, h - 2}
	switch {
	case v.loading:
		put(2, body.y+1, w-2, th.Muted.Render("reading "+v.e.Name+"…"))
	case v.err != "":
		put(2, body.y+1, w-2, th.Danger.Render("✕ ")+th.Muted.Render(v.err))
	case v.as == showImage:
		v.drawPic(t, c, put, body)
	default:
		v.drawText(t, c, put, body)
	}
	put(0, h-1, w, v.footer(th, w))
	return buf.Render()
}

func (t *filesTab) shownDir(side int, d string) string {
	pn := &fpane{side: side, path: d}
	return t.shownPath(pn)
}

func (v *fviewer) drawText(t *filesTab, c *Ctx, put func(x, y, w int, s string), b box) {
	th := c.Theme
	lines, nos := v.shown(b.w, th.Dark)
	numbered := v.as == showText
	gw := 0
	if numbered {
		gw = gutterW(len(v.lines))
	}
	hit := -1
	if len(v.hits) > 0 {
		hit = v.hits[v.hit]
	}
	for y := 0; y < b.h; y++ {
		i := v.top + y
		if i >= len(lines) {
			break
		}
		l := lines[i]
		x := b.x
		if numbered {
			no := ""
			switch {
			case nos == nil:
				no = strconv.Itoa(i + 1)
			case nos[i] > 0:
				no = strconv.Itoa(nos[i])
			}
			st := th.Muted
			if i == hit {
				st = th.Selected
			}
			put(x, b.y+y, gw, st.Render(fmt.Sprintf("%*s", gw-1, no)))
			x += gw + 1
		}
		if v.left > 0 && !v.wrap {
			l = ansi.Cut(l, v.left, v.left+b.w)
		}
		if i == hit {
			l = lipgloss.NewStyle().Background(paletteOf(th).rowSel).Render(ansi.Strip(l))
		}
		put(x, b.y+y, b.w-(x-b.x), l)
	}
	if len(lines) == 0 {
		put(b.x+2, b.y+1, b.w-2, th.Muted.Render("empty"))
	}
}

func (v *fviewer) drawPic(t *filesTab, c *Ctx, put func(x, y, w int, s string), b box) {
	th := c.Theme
	if v.img == nil {
		return
	}
	rows := t.picRows(v.picKey(), v.img, b.w, b.h, th)
	if rows == nil {
		put(b.x+2, b.y+1, b.w-2, th.Muted.Render("…"))
		return
	}
	pw := lipgloss.Width(rows[0])
	x, y := b.x+max(0, (b.w-pw)/2), b.y+max(0, (b.h-len(rows))/2)
	for i, r := range rows {
		put(x, y+i, pw, r)
	}
}

func (v *fviewer) picKey() string { return "v:" + v.full + "@" + v.e.Mod.String() }

func (v *fviewer) footer(th *Theme, w int) string {
	if v.finding {
		return line(w, nil, seg{s: " find: ", fg: th.Amber}, seg{s: v.query + "▏", fg: th.Text}, seg{s: "   ⏎ find · esc stop", fg: th.Dim})
	}
	hints := []string{"esc back"}
	switch v.as {
	case showImage:
		hints = append(hints, "← → the folder's files")
	case showMarkdown:
		hints = append(hints, "↑↓ scroll", "m "+map[bool]string{true: "the page", false: "its text"}[v.plain], "[ ] the folder's files", "/ find")
	default:
		hints = append(hints, "↑↓ scroll", "w "+map[bool]string{true: "unwrap", false: "wrap"}[v.wrap], "[ ] the folder's files", "/ find")
	}
	if len(v.hits) > 0 {
		hints = append(hints, fmt.Sprintf("n N %d of %d", v.hit+1, len(v.hits)))
	}
	if v.side == sideHost {
		hints = append(hints, "d download")
	}
	hints = append(hints, "o open here", "y copy path")
	return " " + hintsFit(th, w-1, hints...)
}
