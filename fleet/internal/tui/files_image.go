package tui

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	xdraw "golang.org/x/image/draw"

	"github.com/charmbracelet/x/ansi/kitty"
)

// Pictures in the terminal. A terminal that speaks kitty's graphics
// protocol (kitty, Ghostty, cmux) is sent the picture once, out of band,
// and shows it wherever cells hold its placeholder character: the cells go
// through the TUI's renderer like any text, so the picture moves, clips and
// clears with the frame. Any other terminal gets it in half blocks, two
// pixels a cell, in its true colors.

// Graphics: how pictures are drawn.
const (
	gfxKitty  = "kitty"
	gfxBlocks = "blocks"
	gfxOff    = "off"
)

// detectGfx is the graphics this terminal has, as its environment tells:
// FLEET_IMAGES chooses; inside tmux, half blocks (it does not pass kitty's
// pictures on unasked).
func detectGfx() string {
	switch v := os.Getenv("FLEET_IMAGES"); v {
	case gfxKitty, gfxBlocks, gfxOff:
		return v
	}
	if os.Getenv("TMUX") != "" || strings.HasPrefix(os.Getenv("TERM"), "screen") {
		return gfxBlocks
	}
	term, prog := os.Getenv("TERM"), strings.ToLower(os.Getenv("TERM_PROGRAM"))
	switch {
	case term == "xterm-kitty", os.Getenv("KITTY_WINDOW_ID") != "",
		term == "xterm-ghostty", prog == "ghostty", os.Getenv("GHOSTTY_RESOURCES_DIR") != "",
		strings.Contains(prog, "cmux"), os.Getenv("CMUX_WORKSPACE_ID") != "":
		return gfxKitty
	}
	return gfxBlocks
}

// kittyQuery asks the terminal whether it shows kitty's pictures; it
// answers OK for image kittyQueryID when it does.
const kittyQueryID = 31

func kittyQuery() tea.Cmd {
	return tea.Raw(fmt.Sprintf("\x1b_Gi=%d,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\", kittyQueryID))
}

// kittyAnswered: a terminal not known by its environment said it shows
// them; one known to lack placeholders (WezTerm, Konsole) is left alone.
func kittyAnswered() bool {
	prog := strings.ToLower(os.Getenv("TERM_PROGRAM"))
	return prog != "wezterm" && os.Getenv("KONSOLE_VERSION") == "" && os.Getenv("TMUX") == ""
}

// pic is a picture as shown: cols×rows cells.
type pic struct {
	img        image.Image // as it came, scaled to what was asked
	w, h       int         // its pixels
	cols, rows int
	id         int      // kitty: the image's id in the terminal
	sent       bool     // kitty: transmitted
	blocks     []string // half blocks: each row's cells
}

// decodePic reads a preview's bytes.
func decodePic(b []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(b))
	return img, err
}

// cellPx is a cell's size in pixels; a usual one when the terminal does
// not say.
func cellPx() (int, int) {
	if w, h := termCellPx(); w > 0 && h > 0 {
		return w, h
	}
	return 9, 18
}

// fitCells is how many cells a w×h-pixel picture takes, inside cols×rows.
func fitCells(w, h, cols, rows, cw, ch int) (int, int) {
	if w <= 0 || h <= 0 || cols <= 0 || rows <= 0 {
		return 0, 0
	}
	c := (w + cw - 1) / cw
	r := (h + ch - 1) / ch
	if c > cols || r > rows { // it came bigger than asked: scale it down
		s := min(float64(cols)/float64(c), float64(rows)/float64(r))
		c, r = max(1, int(float64(c)*s)), max(1, int(float64(r)*s))
	}
	return min(c, cols), min(r, rows)
}

// gallery is the pictures sent to a kitty terminal, by id, so that each
// is sent once and the oldest is deleted when ids run out.
type gallery struct {
	next  int
	order []int
	byKey map[string]*pic
	keyOf map[int]string
}

// Ids 16–255: one is the 256-color index of its placeholders' color, and
// the first 16 are written as plain SGR colors, which mean no picture.
const (
	firstPicID = 16
	lastPicID  = 255
	keepPics   = 40
)

// kittyPic is the picture for key at cols×rows, made from img; the
// command sends it when it is new.
func (g *gallery) kittyPic(key string, img image.Image, cols, rows int) (*pic, tea.Cmd) {
	if g.byKey == nil {
		g.byKey, g.keyOf, g.next = map[string]*pic{}, map[int]string{}, firstPicID
	}
	k := fmt.Sprintf("%s@%dx%d", key, cols, rows)
	if p := g.byKey[k]; p != nil {
		return p, nil
	}
	var del string
	if len(g.order) >= keepPics {
		old := g.order[0]
		g.order = g.order[1:]
		delete(g.byKey, g.keyOf[old])
		delete(g.keyOf, old)
		del = fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", old)
	}
	id := g.next
	for g.keyOf[id] != "" { // in use: the next free
		if id++; id > lastPicID {
			id = firstPicID
		}
	}
	if g.next = id + 1; g.next > lastPicID {
		g.next = firstPicID
	}
	b := img.Bounds()
	p := &pic{img: img, w: b.Dx(), h: b.Dy(), cols: cols, rows: rows, id: id, sent: true}
	g.byKey[k], g.keyOf[id] = p, k
	g.order = append(g.order, id)
	var out strings.Builder
	out.WriteString(del)
	err := kitty.EncodeGraphics(&out, img, &kitty.Options{
		Action: kitty.TransmitAndPut, Transmission: kitty.Direct, Format: kitty.RGBA, Compression: kitty.Zlib,
		ImageWidth: p.w, ImageHeight: p.h, ID: id, Columns: cols, Rows: rows, VirtualPlacement: true,
		Quiet: 2, Chunk: true,
	})
	if err != nil {
		return p, nil
	}
	return p, tea.Raw(out.String())
}

// lookup is the picture for key at cols×rows, when it was sent.
func (g *gallery) lookup(key string, cols, rows int) *pic {
	if g.byKey == nil {
		return nil
	}
	return g.byKey[fmt.Sprintf("%s@%dx%d", key, cols, rows)]
}

// forget deletes every picture sent.
func (g *gallery) forget() tea.Cmd {
	if len(g.order) == 0 {
		return nil
	}
	var out strings.Builder
	for _, id := range g.order {
		fmt.Fprintf(&out, "\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id)
	}
	g.order, g.byKey, g.keyOf = nil, map[string]*pic{}, map[int]string{}
	return tea.Raw(out.String())
}

// kittyRows is the placeholder cells of picture id, cols×rows: each row
// in the id's color, every cell naming its row and column.
func kittyRows(id, cols, rows int) []string {
	out := make([]string, rows)
	color := fmt.Sprintf("\x1b[38;5;%dm", id)
	for r := range rows {
		var b strings.Builder
		b.WriteString(color)
		for c := range cols {
			b.WriteRune(kitty.Placeholder)
			b.WriteRune(kitty.Diacritic(r))
			b.WriteRune(kitty.Diacritic(c))
		}
		b.WriteString("\x1b[39m")
		out[r] = b.String()
	}
	return out
}

// halfBlocks is img in cols×rows cells of ▀, the upper pixel the cell's
// color and the lower its background; see-through pixels over bg.
func halfBlocks(img image.Image, cols, rows int, bg color.Color) []string {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	dst := image.NewRGBA(image.Rect(0, 0, cols, rows*2))
	xdraw.BiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), xdraw.Src, nil)
	br, bgc, bb, _ := bg.RGBA()
	over := func(x, y int) (uint8, uint8, uint8) {
		c := dst.RGBAAt(x, y)
		if c.A == 255 {
			return c.R, c.G, c.B
		}
		a := uint32(c.A)
		mix := func(v uint8, b uint32) uint8 { return uint8((uint32(v)*255 + (b>>8)*(255-a)) / 255) }
		// RGBAAt is premultiplied: v already carries a
		return mix(c.R, br), mix(c.G, bgc), mix(c.B, bb)
	}
	out := make([]string, rows)
	for y := range rows {
		var b strings.Builder
		var lf, lb [3]uint8
		first := true
		for x := range cols {
			r1, g1, b1 := over(x, 2*y)
			r2, g2, b2 := over(x, 2*y+1)
			f, k := [3]uint8{r1, g1, b1}, [3]uint8{r2, g2, b2}
			if first || f != lf {
				fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm", f[0], f[1], f[2])
			}
			if first || k != lb {
				fmt.Fprintf(&b, "\x1b[48;2;%d;%d;%dm", k[0], k[1], k[2])
			}
			lf, lb, first = f, k, false
			b.WriteString("▀")
		}
		b.WriteString("\x1b[m")
		out[y] = b.String()
	}
	return out
}

// blockCells is the cells a w×h-pixel picture takes in half blocks inside
// cols×rows, a cell being cw×ch pixels.
func blockCells(w, h, cols, rows, cw, ch int) (int, int) {
	if w <= 0 || h <= 0 || cols <= 0 || rows <= 0 {
		return 0, 0
	}
	// In cells, the picture is w/cw wide and h/ch tall; fit, keep its shape.
	fw, fh := float64(w)/float64(cw), float64(h)/float64(ch)
	s := min(float64(cols)/fw, float64(rows)/fh, 1) // never bigger than it is
	c, r := max(1, int(fw*s+0.5)), max(1, int(fh*s+0.5))
	return min(c, cols), min(r, rows)
}
