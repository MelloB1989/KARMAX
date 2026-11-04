package tui

import (
	"math"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// box is a rectangle of cells.
type box struct{ x, y, w, h int }

func (b box) in(x, y int) bool { return x >= b.x && x < b.x+b.w && y >= b.y && y < b.y+b.h }

// The smallest pane a split layout makes: a narrower or shorter split shows
// the sessions as tabs.
const (
	minPaneW = 20
	minPaneH = 4 // its head and three lines
)

// paneBoxes is where each of a workspace's n sessions goes in area: each
// box holds its pane's head (its top line) and its terminal under it. In
// tabs (or zoomed, or too small to split) only the focused one has a box;
// the rest have none (w 0). Panes are one cell apart: a column of them is
// divided by a line, a row by the next one's head.
func paneBoxes(layout string, n, focused int, area box, zoom bool) []box {
	out := make([]box, n)
	if n == 0 || area.w <= 0 || area.h <= 0 {
		return out
	}
	focused = min(max(focused, 0), n-1)
	one := func() []box {
		out[focused] = area
		return out
	}
	if zoom || n == 1 || layout == fleetapi.LayoutTabs {
		return one()
	}
	cols := func(a box, k int) []box { // k side by side, a line between
		bs := make([]box, k)
		x := a.x
		for i := range k {
			w := (a.w - (k - 1) - (x - a.x - i)) / (k - i)
			bs[i] = box{x, a.y, w, a.h}
			x += w + 1
		}
		return bs
	}
	rows := func(a box, k int) []box { // k one above another
		bs := make([]box, k)
		y := a.y
		for i := range k {
			h := (a.h - (y - a.y)) / (k - i)
			bs[i] = box{a.x, y, a.w, h}
			y += h
		}
		return bs
	}
	fits := func(bs []box) bool {
		for _, b := range bs {
			if b.w < minPaneW || b.h < minPaneH {
				return false
			}
		}
		return true
	}
	var bs []box
	switch layout {
	case fleetapi.LayoutColumns:
		bs = cols(area, n)
	case fleetapi.LayoutRows:
		bs = rows(area, n)
	case fleetapi.LayoutGrid:
		c := int(math.Ceil(math.Sqrt(float64(n))))
		r := (n + c - 1) / c
		for i, row := range rows(area, r) {
			k := min(c, n-i*c) // the last row's few share its width
			bs = append(bs, cols(row, k)...)
		}
	case fleetapi.LayoutMain:
		mw := area.w * 3 / 5
		bs = append(bs, box{area.x, area.y, mw, area.h})
		bs = append(bs, rows(box{area.x + mw + 1, area.y, area.w - mw - 1, area.h}, n-1)...)
	default:
		return one()
	}
	if !fits(bs) {
		return one()
	}
	copy(out, bs)
	return out
}

// split: the layout shows more than one pane in area.
func split(bs []box) bool {
	n := 0
	for _, b := range bs {
		if b.w > 0 {
			n++
		}
	}
	return n > 1
}

// neighbour is the pane beside from in direction dx, dy (one of them 0):
// the nearest whose span overlaps from's, else -1.
func neighbour(bs []box, from, dx, dy int) int {
	if from < 0 || from >= len(bs) || bs[from].w == 0 {
		return -1
	}
	f := bs[from]
	best, bestD := -1, math.MaxInt
	for i, b := range bs {
		if i == from || b.w == 0 {
			continue
		}
		var d int
		switch {
		case dx > 0 && b.x >= f.x+f.w && overlap(b.y, b.h, f.y, f.h):
			d = b.x - (f.x + f.w)
		case dx < 0 && b.x+b.w <= f.x && overlap(b.y, b.h, f.y, f.h):
			d = f.x - (b.x + b.w)
		case dy > 0 && b.y >= f.y+f.h && overlap(b.x, b.w, f.x, f.w):
			d = b.y - (f.y + f.h)
		case dy < 0 && b.y+b.h <= f.y && overlap(b.x, b.w, f.x, f.w):
			d = f.y - (b.y + b.h)
		default:
			continue
		}
		if d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

func overlap(a, al, b, bl int) bool { return a < b+bl && b < a+al }

// nextLayout is the layout after l, round.
func nextLayout(l string) string {
	for i, x := range fleetapi.Layouts {
		if x == l {
			return fleetapi.Layouts[(i+1)%len(fleetapi.Layouts)]
		}
	}
	return fleetapi.Layouts[0]
}
