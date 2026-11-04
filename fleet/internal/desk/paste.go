package desk

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// What a paste may unpack: a folder's files, in all, and how many.
const (
	maxUnpacked = 4 << 30
	maxEntries  = 100_000
)

// PasteKeep is how long a pasted file is kept.
var PasteKeep = 7 * 24 * time.Hour

// ErrTooLarge is a paste over fleetapi.DeskMaxPaste, or a folder that
// unpacks to more than the desk takes.
var ErrTooLarge = errors.New("too large")

func (m *Manager) pasteDir() string {
	if m.PasteDir != "" {
		return m.PasteDir
	}
	return filepath.Join(filepath.Dir(m.State), "desk-paste")
}

// pasteName is a pasted file's name as written: its base name, with what a
// shell or Claude Code's path detection would trip on made plain.
func pasteName(s string) string {
	s = filepath.Base(strings.ReplaceAll(s, `\`, "/"))
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), strings.ContainsRune("._-+@,=", r):
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	n := strings.Trim(b.String(), "-")
	for strings.Contains(n, "--") {
		n = strings.ReplaceAll(n, "--", "-")
	}
	n = strings.ReplaceAll(n, "-.", ".")
	if n == "" || n == "." || n == ".." {
		n = "pasted"
	}
	if r := []rune(n); len(r) > 120 {
		ext := filepath.Ext(n)
		if len([]rune(ext)) > 12 {
			ext = ""
		}
		n = string(r[:120-len([]rune(ext))]) + ext
	}
	return n
}

// Paste writes what the operator pasted into session: a file, or with tar
// a folder (body a tar, gzipped or not). Each goes in a directory of its
// own, so its name is kept; what is pasted is kept PasteKeep.
func (m *Manager) Paste(ctx context.Context, session, file string, tarred bool, body io.Reader) (fleetapi.DeskPasted, error) {
	m.mu.Lock()
	m.loadAll()
	known := m.find(session) >= 0
	m.mu.Unlock()
	if !known {
		return fleetapi.DeskPasted{}, fmt.Errorf("%w %q", ErrNoSession, session)
	}
	day := m.Now().Format("2006-01-02")
	dir := filepath.Join(m.pasteDir(), day, randHex(4))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fleetapi.DeskPasted{}, err
	}
	name := pasteName(file)
	dst := filepath.Join(dir, name)
	var n int64
	var err error
	if tarred {
		n, err = untar(body, dst)
	} else {
		n, err = writeFile(body, dst)
	}
	if err != nil {
		os.RemoveAll(dir)
		return fleetapi.DeskPasted{}, err
	}
	m.logf("%s: pasted %s (%d bytes)", session, dst, n)
	return fleetapi.DeskPasted{Path: dst, Size: n}, nil
}

func writeFile(body io.Reader, dst string) (int64, error) {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(body, fleetapi.DeskMaxPaste+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > fleetapi.DeskMaxPaste {
		err = fmt.Errorf("%w: more than %d MB", ErrTooLarge, fleetapi.DeskMaxPaste>>20)
	}
	return n, err
}

// untar unpacks a folder's tar into dst: directories and regular files
// only, each inside dst, links and devices passed over.
func untar(body io.Reader, dst string) (int64, error) {
	br := bufio.NewReader(io.LimitReader(body, fleetapi.DeskMaxPaste+1))
	var r io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		z, err := gzip.NewReader(br)
		if err != nil {
			return 0, fmt.Errorf("the folder's gzip: %w", err)
		}
		defer z.Close()
		r = z
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return 0, err
	}
	tr := tar.NewReader(r)
	var total int64
	for entries := 0; ; entries++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, fmt.Errorf("the folder's tar: %w", err)
		}
		if entries >= maxEntries {
			return total, fmt.Errorf("%w: more than %d files", ErrTooLarge, maxEntries)
		}
		rel := filepath.Clean(filepath.FromSlash(h.Name))
		if rel == "." {
			continue
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return total, fmt.Errorf("the folder's tar names %q, outside it", h.Name)
		}
		p := filepath.Join(dst, rel)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return total, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return total, err
			}
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode&0o700|0o600))
			if err != nil {
				return total, err
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxUnpacked-total+1))
			total += n
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return total, err
			}
			if total > maxUnpacked {
				return total, fmt.Errorf("%w: the folder unpacks to more than %d GB", ErrTooLarge, maxUnpacked>>30)
			}
		}
	}
}

// File opens the regular file at path on this machine, for a download.
func (m *Manager) File(path string) (*os.File, os.FileInfo, error) {
	p := m.expand(strings.TrimSpace(path))
	if !filepath.IsAbs(p) {
		return nil, nil, invalidf("%s: not an absolute path", path)
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, missingf("no file %s", p)
		}
		return nil, nil, invalidf("%s: %v", p, err)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, invalidf("%s: not a regular file", p)
	}
	return f, fi, nil
}

// sweep removes the pastes older than PasteKeep, once an hour.
func (m *Manager) sweep() {
	now := m.Now()
	if now.Sub(m.swept) < time.Hour {
		return
	}
	m.swept = now
	days, _ := os.ReadDir(m.pasteDir())
	for _, d := range days {
		t, err := time.ParseInLocation("2006-01-02", d.Name(), now.Location())
		if err != nil || !d.IsDir() {
			continue
		}
		if now.Sub(t.AddDate(0, 0, 1)) > PasteKeep { // the day's last paste is older
			if err := os.RemoveAll(filepath.Join(m.pasteDir(), d.Name())); err != nil {
				m.logf("sweep pastes: %v", err)
			}
		}
	}
}
