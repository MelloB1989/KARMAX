package tui

import (
	"image/color"
	"math"
	"testing"
)

// contrast is the WCAG contrast ratio of two colors, 1 to 21.
func contrast(a, b color.Color) float64 {
	lum := func(c color.Color) float64 {
		r, g, bl, _ := c.RGBA()
		ch := func(v uint32) float64 {
			s := float64(v) / 0xffff
			if s <= 0.04045 {
				return s / 12.92
			}
			return math.Pow((s+0.055)/1.055, 2.4)
		}
		return 0.2126*ch(r) + 0.7152*ch(g) + 0.0722*ch(bl)
	}
	la, lb := lum(a), lum(b)
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

func TestEveryColorStaysVisibleOnALightBackground(t *testing.T) {
	th := NewTheme(false)
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}
	// Text is read; the frame, bars' empty cells and rules only need to be
	// seen (WCAG's 3:1 for text that is bold or a UI part, a little less for
	// a rule under it).
	for _, c := range []struct {
		name string
		c    color.Color
		min  float64
	}{
		{"Text", th.Text, 7}, {"Dim", th.Dim, 4.5}, {"Amber", th.Amber, 3}, {"Cyan", th.Cyan, 4.5},
		{"Alert", th.Alert, 4.5}, {"Frost", th.Frost, 4.5}, {"Green", th.Green, 4.5}, {"Violet", th.Violet, 4.5},
		{"Grey", th.Grey, 3}, {"Line", th.Line, 2.2},
	} {
		for _, bg := range []struct {
			name string
			c    color.Color
		}{{"the theme's ground", th.Ground}, {"white", white}} {
			if got := contrast(c.c, bg.c); got < c.min {
				t.Errorf("%s on %s is %.2f:1, want at least %.1f:1", c.name, bg.name, got, c.min)
			}
		}
	}
}
