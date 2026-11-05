package hostfiles

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/filexfer"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// A folder moves as a tar, either way: Pack writes it, filexfer.Extract
// (whose rules it keeps) unpacks it. Pack is a browser's, not a backup's:
// what it cannot carry (a socket, a file it may not read, a link out of
// the folder) it leaves out and counts, rather than fail the whole copy.

// Pack writes the file or folder at root as a tar under its own name. n,
// when set, counts the bytes of files written. It answers how many entries
// it left out.
func Pack(ctx context.Context, w io.Writer, root string, n *atomic.Int64) (skipped int, err error) {
	root = filepath.Clean(root)
	fi, err := os.Lstat(root)
	if err != nil {
		return 0, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 { // the thing named, not a link to it
		if fi, err = os.Stat(root); err != nil {
			return 0, err
		}
	}
	tw := tar.NewWriter(w)
	base := filepath.Base(root)
	if !fi.IsDir() {
		ok, err := packEntry(tw, root, base, fi, n)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 1, errors.New(root + ": not a file this can copy")
		}
		return 0, tw.Close()
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if p == root {
				return err
			}
			skipped++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := path.Join(base, filepath.ToSlash(rel))
		fi, err := d.Info()
		if err != nil {
			skipped++
			return nil
		}
		if fi.Mode()&fs.ModeSymlink != 0 && !inside(name, p) {
			skipped++
			return nil
		}
		ok, err := packEntry(tw, p, name, fi, n)
		if err != nil {
			return err
		}
		if !ok {
			skipped++
		}
		return nil
	})
	if err != nil {
		return skipped, err
	}
	return skipped, tw.Close()
}

// inside: the symlink at p, named name in the tar, is relative and stays
// in the tar's tree.
func inside(name, p string) bool {
	to, err := os.Readlink(p)
	if err != nil || to == "" || filepath.IsAbs(to) {
		return false
	}
	j := path.Clean(path.Join(path.Dir(name), filepath.ToSlash(to)))
	top := strings.SplitN(name, "/", 2)[0]
	return j == top || strings.HasPrefix(j, top+"/")
}

// packEntry writes one entry; false: left out (not a file, a folder or a
// link, or a file it may not read).
func packEntry(tw *tar.Writer, file, name string, fi fs.FileInfo, n *atomic.Int64) (bool, error) {
	h := &tar.Header{Name: name, Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime()}
	var f *os.File
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		to, err := os.Readlink(file)
		if err != nil {
			return false, nil
		}
		h.Typeflag, h.Linkname = tar.TypeSymlink, filepath.ToSlash(to)
	case fi.IsDir():
		h.Typeflag, h.Name = tar.TypeDir, name+"/"
	case fi.Mode().IsRegular():
		var err error
		if f, err = os.Open(file); err != nil {
			return false, nil
		}
		defer f.Close()
		h.Typeflag, h.Size = tar.TypeReg, fi.Size()
	default:
		return false, nil
	}
	if err := tw.WriteHeader(h); err != nil {
		return false, err
	}
	if f == nil {
		return true, nil
	}
	// A file that shrank as it was read is padded, one that grew cut: the
	// archive stays well formed either way.
	c, err := io.Copy(countWriter{tw, n}, io.LimitReader(f, h.Size))
	if err != nil {
		return false, err
	}
	if c < h.Size {
		if _, err := io.CopyN(countWriter{tw, n}, zeros{}, h.Size-c); err != nil {
			return false, err
		}
	}
	return true, nil
}

type countWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countWriter) Write(p []byte) (int, error) {
	k, err := c.w.Write(p)
	if c.n != nil {
		c.n.Add(int64(k))
	}
	return k, err
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Count is what the files under root hold, when it finds out within
// budget; -1 when it does not.
func Count(root string, budget time.Duration) int64 {
	fi, err := os.Stat(root)
	if err != nil {
		return -1
	}
	if !fi.IsDir() {
		return fi.Size()
	}
	until := time.Now().Add(budget)
	var total int64
	seen := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != root {
				return fs.SkipDir
			}
			return nil
		}
		if seen++; seen%256 == 0 && time.Now().After(until) {
			return errStop
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	if err != nil {
		return -1
	}
	return total
}

var errStop = errors.New("stop")

// Tar writes the file or folder at p as a tar (gzipped when gz) to w.
func (m *Manager) Tar(ctx context.Context, p string, gz bool, w io.Writer) error {
	abs, err := m.Abs(p)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return osErr(err, abs)
	}
	bw := bufio.NewWriterSize(w, 256<<10)
	var out io.Writer = bw
	var z *gzip.Writer
	if gz {
		z, _ = gzip.NewWriterLevel(bw, gzip.BestSpeed)
		out = z
	}
	skipped, err := Pack(ctx, out, abs, nil)
	if err != nil {
		return err
	}
	if skipped > 0 {
		m.logf("tar %s: left out %d entries", abs, skipped)
	}
	if z != nil {
		if err := z.Close(); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// Untar unpacks a tar (gzipped or not) of a folder into dir, under its own
// name, or "name 2" when that is taken; the stream is unpacked beside the
// place first, so that a broken one leaves nothing half there.
func (m *Manager) Untar(dir string, body io.Reader) (fleetapi.FilePut, error) {
	m.init()
	var out fleetapi.FilePut
	d, err := m.Abs(dir)
	if err != nil {
		return out, err
	}
	if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
		if err != nil {
			return out, osErr(err, d)
		}
		return out, failf(ErrInvalid, "%s: not a folder", d)
	}
	tmp, err := os.MkdirTemp(d, ".fleet-untar-")
	if err != nil {
		return out, osErr(err, d)
	}
	defer os.RemoveAll(tmp)
	r, err := Ungzip(body)
	if err != nil {
		return out, failf(ErrInvalid, "the folder's tar: %v", err)
	}
	limit := int64(-1)
	if free := freeSpace(d); free >= 0 {
		limit = max(free-reserve, 1)
	}
	cr := &countReader{r: filexfer.Limit(r, limit)}
	if err := filexfer.Extract(tmp, cr); err != nil {
		if errors.Is(err, filexfer.ErrTooLarge) {
			return out, failf(ErrTooLarge, "the folder does not fit on fleetd's disk")
		}
		return out, failf(ErrInvalid, "the folder's tar: %v", err)
	}
	tops, err := os.ReadDir(tmp)
	if err != nil {
		return out, err
	}
	for _, t := range tops {
		dest := unique(d, t.Name())
		if err := os.Rename(filepath.Join(tmp, t.Name()), dest); err != nil {
			return out, osErr(err, dest)
		}
		if out.Path == "" {
			out.Path = dest
		}
	}
	if out.Path == "" {
		return out, failf(ErrInvalid, "the tar held nothing")
	}
	out.Size, out.Have, out.Done = cr.n, cr.n, true
	return out, nil
}

// Ungzip is r, gunzipped when it starts as gzip does.
func Ungzip(r io.Reader) (io.Reader, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(2)
	if len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
		return gzip.NewReader(br)
	}
	return br, nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += int64(k)
	return k, err
}
