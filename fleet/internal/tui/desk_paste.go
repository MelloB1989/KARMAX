package tui

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/MelloB1989/karmax/fleet/internal/clipboard"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// The desk's sessions run on fleetd's machine, the terminal on this one: a
// file pasted or dropped here is sent there first, and its path there is
// what is typed. Claude Code takes an image's path as the image.

// pasteTimeout bounds one paste: a big file on a slow link.
const pasteTimeout = 30 * time.Minute

// deskPastedMsg is a paste sent: the paths to type into session, or why
// not. kind none is a Ctrl+V with neither files nor an image on the
// clipboard: the key itself goes to the session.
type deskPastedMsg struct {
	session string
	kind    string // files | image | none
	paths   []string
	names   []string
	bytes   int64
	err     error
}

// localPaths is the pasted text as paths of files and folders here, as a
// terminal pastes what is dropped on it or copied in a file manager: each
// absolute (or under ~), escaped with backslashes or quoted, apart by
// spaces or lines. Text that is anything else is nil: it is typed as it is.
func localPaths(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 64<<10 || !strings.ContainsAny(text[:1], "/~'\"") && !strings.HasPrefix(text, "file://") {
		return nil
	}
	words, ok := shellWords(text)
	if !ok || len(words) == 0 {
		return nil
	}
	home, _ := os.UserHomeDir()
	var out []string
	for _, w := range words {
		if strings.HasPrefix(w, "file://") {
			w = strings.TrimPrefix(w, "file://")
		}
		if w == "~" || strings.HasPrefix(w, "~/") {
			if home == "" {
				return nil
			}
			w = home + w[1:]
		}
		if !filepath.IsAbs(w) {
			return nil
		}
		if _, err := os.Stat(w); err != nil {
			return nil
		}
		out = append(out, w)
	}
	return out
}

// shellWords splits s as a shell would its words: backslash escapes,
// single and double quotes; false for an unclosed quote.
func shellWords(s string) ([]string, bool) {
	var out []string
	var cur strings.Builder
	in := false // a word is under way
	var quote rune
	esc := false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc, in = false, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' {
				esc = true
			} else {
				cur.WriteRune(r)
			}
		case r == '\\':
			esc, in = true, true
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 || esc {
		return nil, false
	}
	if in {
		out = append(out, cur.String())
	}
	return out, true
}

// pasteFiles sends local files and folders to fleetd for session, and
// answers the paths to type. local (the TUI runs on fleetd's machine): the
// paths are typed as they are.
func pasteFiles(cl fleetapi.Client, session string, files []string, local bool) tea.Cmd {
	return func() tea.Msg {
		out := deskPastedMsg{session: session, kind: "files"}
		ctx, cancel := context.WithTimeout(context.Background(), pasteTimeout)
		defer cancel()
		for _, f := range files {
			out.names = append(out.names, filepath.Base(f))
			if local {
				out.paths = append(out.paths, f)
				continue
			}
			p, err := sendPath(ctx, cl, session, f, filepath.Base(f))
			if err != nil {
				out.err = fmt.Errorf("%s: %w", filepath.Base(f), err)
				return out
			}
			out.paths, out.bytes = append(out.paths, p.Path), out.bytes+p.Size
		}
		return out
	}
}

// ctrlV is Claude Code's paste-an-image key, made to reach across: what
// this machine's clipboard holds, files or an image, goes to fleetd, and
// its paths are typed; with neither (or no clipboard here) kind is none.
func ctrlV(cl fleetapi.Client, clip clipboard.Clipboard, session string, local bool) tea.Cmd {
	return func() tea.Msg {
		out := deskPastedMsg{session: session, kind: "none"}
		if clip == nil {
			return out
		}
		tmp, err := os.MkdirTemp("", "fleet-paste-")
		if err != nil {
			out.err = err
			return out
		}
		defer os.RemoveAll(tmp)
		ctx, cancel := context.WithTimeout(context.Background(), pasteTimeout)
		defer cancel()
		got, err := clip.Read(ctx, filepath.Join(tmp, "clipboard.png"))
		switch {
		case err != nil:
			out.err = fmt.Errorf("the clipboard: %w", err)
			return out
		case len(got.Files) > 0:
			return pasteFiles(cl, session, got.Files, local)()
		case got.Image != "":
			out.kind = "image"
			name := "clipboard-" + time.Now().Format("150405") + ".png"
			p, err := sendPath(ctx, cl, session, got.Image, name)
			if err != nil {
				out.err = fmt.Errorf("the image: %w", err)
				return out
			}
			out.paths, out.names, out.bytes = []string{p.Path}, []string{name}, p.Size
		}
		return out
	}
}

// sendPath sends the file or folder at local as name.
func sendPath(ctx context.Context, cl fleetapi.Client, session, local, name string) (*fleetapi.DeskPasted, error) {
	fi, err := os.Stat(local)
	if err != nil {
		return nil, err
	}
	switch {
	case fi.Mode().IsRegular():
		if fi.Size() > fleetapi.DeskMaxPaste {
			return nil, fmt.Errorf("%s: the desk takes %s at most", bytesOf(fi.Size()), bytesOf(fleetapi.DeskMaxPaste))
		}
		f, err := os.Open(local)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return cl.DeskPaste(ctx, session, name, f, fi.Size(), false)
	case fi.IsDir():
		n, err := dirSize(local)
		if err != nil {
			return nil, err
		}
		if n > fleetapi.DeskMaxPaste {
			return nil, fmt.Errorf("the folder is %s: the desk takes %s at most", bytesOf(n), bytesOf(fleetapi.DeskMaxPaste))
		}
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(tarDir(local, pw)) }()
		defer pr.Close()
		return cl.DeskPaste(ctx, session, name, pr, -1, true)
	}
	return nil, fmt.Errorf("not a file or a folder")
}

func dirSize(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type().IsRegular() {
			if fi, err := e.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n, err
}

// tarDir writes dir's directories and regular files, gzipped, to w; links
// and the rest are left out, and nothing of this machine's owners.
func tarDir(dir string, w io.Writer) error {
	z, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
	t := tar.NewWriter(z)
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		if !e.IsDir() && !e.Type().IsRegular() {
			return nil
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		h, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if e.IsDir() {
			h.Name += "/"
		}
		h.Uname, h.Gname, h.Uid, h.Gid = "", "", 0, 0
		if err := t.WriteHeader(h); err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.CopyN(t, f, h.Size)
		return err
	})
	if err != nil {
		return err
	}
	if err := t.Close(); err != nil {
		return err
	}
	return z.Close()
}

// typedPaths is paths as typed into a session: each escaped for a shell
// (Claude Code reads them so too), a space after the last.
func typedPaths(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		for _, r := range p {
			if strings.ContainsRune(" \t'\"\\$`!&;|<>()[]{}*?#~", r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		b.WriteByte(' ')
	}
	return b.String()
}

// bytesOf is a size in a few characters.
func bytesOf(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
