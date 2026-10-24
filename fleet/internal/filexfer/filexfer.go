// Package filexfer moves files between a person's machine and an agent's
// container as a tar stream, and keeps that stream inside its tree.
//
// Nothing here trusts a stream. Pack, Sanitize and Extract all enforce the
// same rules, so an archive is checked by whoever is about to act on it: the
// client packing it, fleetd on the way into a container or out of one, and
// the client unpacking it:
//
//   - only regular files, directories and symlinks; no hard links, devices
//     or fifos;
//   - names are relative and stay in the tree: no absolute path, no "..";
//   - a symlink points somewhere inside the tree, relative, never absolute;
//   - nothing is written through a symlink the archive itself made;
//   - modes are kept but for setuid, setgid and sticky; owners are dropped.
package filexfer

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Roots are where a file may land inside a container, and be read from.
var Roots = []string{"/work", "/tmp"}

// DefaultDir is where files go when no directory is named.
const DefaultDir = "/work/inbox"

// ErrTooLarge is a transfer over the size cap.
var ErrTooLarge = errors.New("transfer is over the size limit")

// CheckPath cleans a path inside a container and refuses one that is not
// under a root. A relative path is taken from /work.
func CheckPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("path %q: not a path", p)
	}
	if !path.IsAbs(p) {
		p = path.Join("/work", p)
	}
	p = path.Clean(p)
	for _, r := range Roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return p, nil
		}
	}
	return "", fmt.Errorf("path %s: files go to and from %s only", p, strings.Join(Roots, " or "))
}

// Limit reads at most max bytes of r, then fails with ErrTooLarge
// (max <= 0: no limit). The stream is cut, never read into memory.
func Limit(r io.Reader, max int64) io.Reader {
	if max <= 0 {
		return r
	}
	return &limited{r: r, left: max, max: max}
}

type limited struct {
	r         io.Reader
	left, max int64
}

func (l *limited) Read(p []byte) (int, error) {
	if l.left <= 0 {
		// Past the cap: only a stream that ends exactly there is fine.
		var one [1]byte
		if n, _ := l.r.Read(one[:]); n > 0 {
			return 0, fmt.Errorf("%w (%s)", ErrTooLarge, Size(l.max))
		}
		return 0, io.EOF
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}

// Size is bytes, as people say it: 2 GiB, 512 KiB.
func Size(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// tree checks a stream's entries one by one, remembering the symlinks it has
// seen so that no later entry passes through one.
type tree struct {
	seen  map[string]bool
	links map[string]bool
}

func newTree() *tree { return &tree{seen: map[string]bool{}, links: map[string]bool{}} }

// entry validates a header and returns its clean name.
func (t *tree) entry(h *tar.Header) (string, error) {
	name := strings.TrimSuffix(h.Name, "/")
	if name == "" || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive entry %q: not a name inside the tree", h.Name)
	}
	name = path.Clean(name)
	if name == "." {
		return "", nil // the tree's own root: nothing to make
	}
	if name == ".." || strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("archive entry %q leaves the tree", h.Name)
	}
	for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
		if t.links[dir] {
			return "", fmt.Errorf("archive entry %q is written through the symlink %s", h.Name, dir)
		}
	}
	if t.links[name] {
		return "", fmt.Errorf("archive entry %q replaces a symlink the archive made", h.Name)
	}
	switch h.Typeflag {
	case tar.TypeReg, tar.TypeDir:
	case tar.TypeSymlink:
		if t.seen[name] {
			return "", fmt.Errorf("archive entry %q: a symlink over something already in the archive", h.Name)
		}
		if h.Linkname == "" || strings.ContainsRune(h.Linkname, 0) || path.IsAbs(h.Linkname) {
			return "", fmt.Errorf("symlink %s points to %q: only relative links inside the tree are copied", name, h.Linkname)
		}
		if to := path.Join(path.Dir(name), h.Linkname); to == ".." || strings.HasPrefix(to, "../") {
			return "", fmt.Errorf("symlink %s points to %q, outside the tree", name, h.Linkname)
		}
		t.links[name] = true
	default:
		return "", fmt.Errorf("archive entry %q is not a file, directory or symlink", h.Name)
	}
	for dir := name; dir != "."; dir = path.Dir(dir) {
		t.seen[dir] = true // a symlink may not come over a directory made for its files
	}
	return name, nil
}

// next reads the next header that is an entry (not a pax global header).
func next(tr *tar.Reader) (*tar.Header, error) {
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if h.Typeflag == '\x00' {
			h.Typeflag = tar.TypeReg
		}
		return h, nil
	}
}

func perm(h *tar.Header) int64 { return h.Mode & 0o777 }

// Sanitize copies a tar stream from src to dst, entry by entry, refusing
// what the rules at the top forbid. The copy has no owners and no special
// mode bits. It streams: only one entry's header is held at a time. A
// refusal is returned, and dst is left without the end of the archive.
func Sanitize(dst io.Writer, src io.Reader) error {
	tr, tw, t := tar.NewReader(src), tar.NewWriter(dst), newTree()
	for {
		h, err := next(tr)
		if errors.Is(err, io.EOF) {
			return tw.Close()
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		name, err := t.entry(h)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		out := &tar.Header{Typeflag: h.Typeflag, Name: name, Mode: perm(h), ModTime: h.ModTime, Format: tar.FormatPAX}
		switch h.Typeflag {
		case tar.TypeDir:
			out.Name += "/"
		case tar.TypeSymlink:
			out.Linkname = h.Linkname
		case tar.TypeReg:
			out.Size = h.Size
		}
		if err := tw.WriteHeader(out); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}
}

// Extract unpacks a tar stream into dir, which it makes. It holds the same
// rules as Sanitize, and writes through an os.Root, so that even a symlink
// that was in dir before cannot take a file outside it.
func Extract(dir string, src io.Reader) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr, t := tar.NewReader(src), newTree()
	for {
		h, err := next(tr)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		name, err := t.entry(h)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		mode := fs.FileMode(perm(h))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := mkdirAll(root, name); err != nil {
				return err
			}
			// A directory's own mode may not let us write into it; it is
			// set once its files are in (setDirModes), and until then it is
			// at least ours to use.
			_ = root.Chmod(name, mode|0o700)
		case tar.TypeSymlink:
			if err := mkdirAll(root, path.Dir(name)); err != nil {
				return err
			}
			_ = root.Remove(name)
			if err := root.Symlink(h.Linkname, name); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := mkdirAll(root, path.Dir(name)); err != nil {
				return err
			}
			_ = root.Remove(name)
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode|0o200)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			_ = root.Chmod(name, mode)
			_ = root.Chtimes(name, h.ModTime, h.ModTime)
		}
		if h.Typeflag == tar.TypeDir {
			defer func(name string, mode fs.FileMode) { _ = root.Chmod(name, mode) }(name, mode)
		}
	}
}

func mkdirAll(root *os.Root, name string) error {
	if name == "." || name == "" {
		return nil
	}
	return root.MkdirAll(name, 0o755)
}

// Pack writes the named local files and directories as a tar stream. Each
// is stored under its own base name, directories with everything below
// them. Symlinks are stored as links, never followed; one that points
// outside the tree it is in is refused, so a copy cannot carry a way out.
func Pack(w io.Writer, paths []string) error {
	tw := tar.NewWriter(w)
	for _, p := range paths {
		p = filepath.Clean(p)
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		fi, err := os.Lstat(abs)
		if err != nil {
			return err
		}
		base := filepath.Base(abs)
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			if err := packOne(tw, abs, base, fi); err != nil {
				return err
			}
			continue
		}
		err = filepath.WalkDir(abs, func(file string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(abs, file)
			if err != nil {
				return err
			}
			name := path.Join(base, filepath.ToSlash(rel))
			fi, err := d.Info()
			if err != nil {
				return err
			}
			return packOne(tw, file, name, fi)
		})
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// packOne writes one file under name.
func packOne(tw *tar.Writer, file, name string, fi fs.FileInfo) error {
	h := &tar.Header{Name: name, Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime()}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		to, err := os.Readlink(file)
		if err != nil {
			return err
		}
		h.Typeflag, h.Linkname = tar.TypeSymlink, filepath.ToSlash(to)
		if _, err := newTree().entry(h); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
	case fi.IsDir():
		h.Typeflag, h.Name = tar.TypeDir, name+"/"
	case fi.Mode().IsRegular():
		h.Typeflag, h.Size = tar.TypeReg, fi.Size()
	default:
		return fmt.Errorf("%s: not a file, directory or symlink", file)
	}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	if h.Typeflag != tar.TypeReg {
		return nil
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	// A file that grew since its size was read is cut at that size, one
	// that shrank is an error: either way the archive stays well formed.
	n, err := io.Copy(tw, io.LimitReader(f, h.Size))
	if err == nil && n < h.Size {
		err = fmt.Errorf("%s: shrank while it was being read", file)
	}
	return err
}
