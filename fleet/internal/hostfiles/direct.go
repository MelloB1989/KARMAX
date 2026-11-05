package hostfiles

import (
	"context"
	"io"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Direct is fleetapi.Files over m in this process: the fake's files, in
// tests and the demo.
func Direct(m *Manager) fleetapi.Files { return direct{m} }

type direct struct{ m *Manager }

func (d direct) FilesList(_ context.Context, path string) (*fleetapi.FileDir, error) {
	l, err := d.m.List(path)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (d direct) FilesRead(_ context.Context, path string, off, n int64, ifMod time.Time) (*fleetapi.FileRead, error) {
	f, fi, err := d.m.Open(path)
	if err != nil {
		return nil, err
	}
	if !ifMod.IsZero() && !fi.ModTime().Truncate(time.Second).Equal(ifMod.Truncate(time.Second)) {
		off = 0 // changed since: from the start
	}
	if off > fi.Size() {
		off = fi.Size()
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	var r io.Reader = f
	if n >= 0 {
		r = io.LimitReader(f, n)
	}
	return &fleetapi.FileRead{Body: readCloser{r, f}, From: off, Size: fi.Size(), Mod: fi.ModTime(), Route: fleetapi.RouteLocal}, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

func (d direct) FilesPreview(ctx context.Context, path string, w, h int) ([]byte, error) {
	b, _, err := d.m.Preview(ctx, path, w, h)
	return b, err
}

func (d direct) FilesTar(ctx context.Context, path string) (*fleetapi.FileRead, error) {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(d.m.Tar(ctx, path, false, pw)) }()
	abs, _ := d.m.Abs(path)
	return &fleetapi.FileRead{Body: pr, Size: Count(abs, time.Second), Route: fleetapi.RouteLocal}, nil
}

func (d direct) FilesPut(_ context.Context, dir, name, id string, off, size int64, body io.Reader) (*fleetapi.FilePut, error) {
	p, err := d.m.Put(dir, name, id, off, size, body)
	return &p, err
}

func (d direct) FilesHave(_ context.Context, id string) (int64, error) { return d.m.Have(id) }

func (d direct) FilesUntar(_ context.Context, dir string, body io.Reader) (*fleetapi.FilePut, error) {
	p, err := d.m.Untar(dir, body)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (d direct) FilesOp(_ context.Context, op fleetapi.FileOp) (*fleetapi.FileOpDone, error) {
	o, err := d.m.Op(op)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (d direct) FilesRoute() string { return fleetapi.RouteLocal }
