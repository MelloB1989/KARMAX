package tui

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"charm.land/glamour/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/x/ansi"
)

// A file's text as shown: code in its colors, markdown as a page, other
// bytes as hex.

// What a file is, to show it.
const (
	showText     = "text"
	showMarkdown = "markdown"
	showImage    = "image"
	showHex      = "hex"
	showNone     = "none"
)

var imageExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true,
	".tif": true, ".tiff": true}

// pictured: a file shown as a picture (one fleetd can make: images, and
// videos and PDFs when its machine has ffmpeg or pdftoppm).
func pictured(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return imageExt[ext] || ext == ".pdf" || ext == ".mp4" || ext == ".mov" || ext == ".m4v" || ext == ".mkv" || ext == ".webm" || ext == ".avi"
}

// showAs is how a file named name, starting with head, is shown.
func showAs(name string, head []byte) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case pictured(name):
		return showImage
	case ext == ".md" || ext == ".markdown" || ext == ".mdx":
		return showMarkdown
	case len(head) == 0:
		return showText
	case binary(head):
		return showHex
	}
	return showText
}

// binary: bytes that are not text (a NUL, or much that is not UTF-8).
func binary(b []byte) bool {
	if len(b) > 8<<10 {
		b = b[:8<<10]
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	bad := 0
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			if len(b) < 4 { // cut mid-rune at the end of what was read
				break
			}
			bad++
		}
		b = b[n:]
	}
	return bad > 8
}

// plainLines is text as lines: tabs as spaces, no control bytes but color.
func plainLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	s = strings.TrimSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.Map(func(r rune) rune {
			if r < 0x20 && r != 0x1b || r == 0x7f {
				return '·'
			}
			if r == 0x1b {
				return '␛'
			}
			return r
		}, l)
	}
	return lines
}

// highlightMax: text longer than this is shown plain (colouring it takes
// longer than reading it).
const highlightMax = 512 << 10

// highlight is text's lines in its language's colors, by its name or its
// first line; plain when there is no language for it.
func highlight(name, text string, dark bool) []string {
	plain := plainLines(text)
	if len(text) > highlightMax {
		return plain
	}
	lx := lexers.Match(filepath.Base(name))
	if lx == nil {
		lx = lexers.Analyse(text)
	}
	if lx == nil || lx.Config().Name == "plaintext" {
		return plain
	}
	lx = chroma.Coalesce(lx)
	st := styles.Get("github-dark")
	if !dark {
		st = styles.Get("github")
	}
	f := formatters.Get("terminal16m")
	src := strings.Join(plain, "\n")
	it, err := lx.Tokenise(nil, src)
	if err != nil {
		return plain
	}
	var b bytes.Buffer
	if err := f.Format(&b, st, it); err != nil {
		return plain
	}
	out := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(out) != len(plain) { // the formatter split it otherwise: show it plain
		return plain
	}
	// A style that spans lines (a block comment) is closed at each end of
	// line by the formatter's own reset; carry it over so the next starts
	// in it too.
	return carryStyles(out)
}

// carryStyles opens each line with the SGR the line before it ended in.
func carryStyles(lines []string) []string {
	open := ""
	for i, l := range lines {
		if open != "" {
			lines[i] = open + l
		}
		if j := strings.LastIndex(l, "\x1b["); j >= 0 {
			k := strings.IndexByte(l[j:], 'm')
			if k > 0 {
				sgr := l[j : j+k+1]
				if sgr == "\x1b[0m" || sgr == "\x1b[m" {
					open = ""
				} else {
					open = sgr
				}
			}
		}
		lines[i] += "\x1b[0m"
	}
	return lines
}

// markdown is md as a page w wide.
func markdown(md string, w int, dark bool) []string {
	style := "dark"
	if !dark {
		style = "light"
	}
	r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(max(20, w-2)))
	if err != nil {
		return plainLines(md)
	}
	out, err := r.Render(md)
	if err != nil {
		return plainLines(md)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[0])) == "" {
		lines = lines[1:]
	}
	return lines
}

// hexLines is b as a hex dump: offset, 16 bytes, and those that print.
func hexLines(b []byte, base int64) []string {
	var out []string
	for i := 0; i < len(b); i += 16 {
		row := b[i:min(i+16, len(b))]
		var h, a strings.Builder
		for j := range 16 {
			if j == 8 {
				h.WriteByte(' ')
			}
			if j < len(row) {
				fmt.Fprintf(&h, "%02x ", row[j])
				if row[j] >= 0x20 && row[j] < 0x7f {
					a.WriteByte(row[j])
				} else {
					a.WriteByte('.')
				}
			} else {
				h.WriteString("   ")
			}
		}
		out = append(out, fmt.Sprintf("%08x  %s %s", base+int64(i), h.String(), a.String()))
	}
	return out
}
