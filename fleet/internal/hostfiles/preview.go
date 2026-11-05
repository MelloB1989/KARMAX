package hostfiles

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// A preview is a picture of a file, made on fleetd's machine at the size
// it is shown, so that a 12 MB photo crosses a slow link as 60 KB: images
// Go reads (PNG, JPEG, GIF, WebP, BMP, TIFF) turned the way their camera
// says, a video's frame by ffmpeg, a PDF's first page by pdftoppm, when
// those are there.

// Bounds of a preview.
const (
	maxPreviewSide = 4096
	maxSourcePix   = 100 << 20 // pixels: a bigger image is not decoded
	toolTimeout    = 20 * time.Second
)

// Preview is a picture of the file at p that fits w×h pixels: its bytes and
// their type (image/png or image/jpeg).
func (m *Manager) Preview(ctx context.Context, p string, w, h int) ([]byte, string, error) {
	m.init()
	abs, err := m.Abs(p)
	if err != nil {
		return nil, "", err
	}
	w, h = clampSide(w), clampSide(h)
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	switch ext := strings.ToLower(filepath.Ext(abs)); {
	case videoExt[ext]:
		return m.byTool(ctx, abs, w, h, "ffmpeg", "-v", "error", "-ss", "1", "-i", abs, "-frames:v", "1",
			"-vf", "scale="+strconv.Itoa(w)+":"+strconv.Itoa(h)+":force_original_aspect_ratio=decrease",
			"-f", "image2pipe", "-vcodec", "png", "-")
	case ext == ".pdf":
		return m.byTool(ctx, abs, w, h, "pdftoppm", "-f", "1", "-l", "1", "-png", "-scale-to", strconv.Itoa(max(w, h)), "-singlefile", abs)
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, "", osErr(err, abs)
	}
	defer f.Close()
	return picture(f, w, h)
}

func clampSide(n int) int { return min(max(n, 16), maxPreviewSide) }

var videoExt = map[string]bool{".mp4": true, ".mov": true, ".m4v": true, ".mkv": true, ".webm": true, ".avi": true}

// picture decodes an image from f and scales it to fit w×h.
func picture(f io.ReadSeeker, w, h int) ([]byte, string, error) {
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return nil, "", failf(ErrInvalid, "not an image this can show: %v", err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxSourcePix {
		return nil, "", failf(ErrTooLarge, "%d×%d is too big to preview", cfg.Width, cfg.Height)
	}
	orient := 1
	if format == "jpeg" {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			orient = exifOrientation(f)
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, "", failf(ErrInvalid, "reading the image: %v", err)
	}
	if orient >= 5 { // turned a quarter: it is fit the other way round
		w, h = h, w
	}
	out := orient8(fit(src, w, h), orient)
	var b bytes.Buffer
	if format == "jpeg" || opaque(out) && format != "png" && format != "gif" {
		err = jpeg.Encode(&b, out, &jpeg.Options{Quality: 85})
		return b.Bytes(), "image/jpeg", err
	}
	err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&b, out)
	return b.Bytes(), "image/png", err
}

// fit is src scaled down (never up) to fit w×h.
func fit(src image.Image, w, h int) image.Image {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= w && sh <= h {
		return src
	}
	scale := min(float64(w)/float64(sw), float64(h)/float64(sh))
	dw, dh := max(1, int(float64(sw)*scale+0.5)), max(1, int(float64(sh)*scale+0.5))
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	// BiLinear widens as it shrinks, so edges and text do not alias.
	xdraw.BiLinear.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)
	return dst
}

func opaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	return false
}

// byTool is a picture by an outside program writing a PNG to stdout.
func (m *Manager) byTool(ctx context.Context, abs string, w, h int, tool string, args ...string) ([]byte, string, error) {
	bin := lookTool(tool, m.Home)
	if bin == "" {
		return nil, "", failf(ErrInvalid, "no %s on fleetd's machine to preview %s", tool, filepath.Base(abs))
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, "", osErr(err, abs)
	}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &limitBuf{b: &errb, n: 4096}
	if err := cmd.Run(); err != nil || out.Len() == 0 {
		msg := strings.TrimSpace(errb.String())
		if msg == "" && err != nil {
			msg = err.Error()
		}
		return nil, "", failf(ErrInvalid, "%s could not preview %s: %s", tool, filepath.Base(abs), firstLine(msg))
	}
	b, typ, err := picture(bytes.NewReader(out.Bytes()), w, h)
	return b, typ, err
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

type limitBuf struct {
	b *bytes.Buffer
	n int
}

func (l *limitBuf) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// lookTool finds a program on PATH, or where a user's own installs go
// (fleetd's service PATH is short).
func lookTool(name, home string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range []string{filepath.Join(home, ".local", "bin"), "/usr/local/bin", "/opt/homebrew/bin", "/usr/bin"} {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// exifOrientation is a JPEG's EXIF orientation (1–8); 1 when it has none.
func exifOrientation(r io.Reader) int {
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(r, head)
	b := head[:n]
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		if marker == 0xDA || size < 2 { // the image data: no EXIF before it
			return 1
		}
		seg := b[i+4 : min(len(b), i+2+size)]
		if marker == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + size
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	ifd := int(bo.Uint32(t[4:]))
	if ifd+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[ifd:]))
	for k := 0; k < n; k++ {
		e := ifd + 2 + 12*k
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient8 is img turned upright by EXIF orientation o.
func orient8(img image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	ow, oh := w, h
	if o >= 5 {
		ow, oh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, ow, oh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
