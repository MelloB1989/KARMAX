package fleetapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// The fake's files are HostFiles's: a test points it at a directory.

var errNoFiles = errors.New("the fake has no files (Fake.HostFiles)")

func (f *Fake) files(call string) (Files, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, call)
	role, hf := f.Role, f.HostFiles
	f.mu.Unlock()
	if role != RoleAdmin {
		return nil, fmt.Errorf("403: files are for admins")
	}
	if hf == nil {
		return nil, errNoFiles
	}
	return hf, nil
}

func (f *Fake) FilesList(ctx context.Context, path string) (*FileDir, error) {
	hf, err := f.files("files " + path)
	if err != nil {
		return nil, err
	}
	return hf.FilesList(ctx, path)
}

func (f *Fake) FilesRead(ctx context.Context, path string, off, n int64, ifMod time.Time) (*FileRead, error) {
	hf, err := f.files(fmt.Sprintf("read %s %d", path, off))
	if err != nil {
		return nil, err
	}
	return hf.FilesRead(ctx, path, off, n, ifMod)
}

func (f *Fake) FilesPreview(ctx context.Context, path string, w, h int) ([]byte, error) {
	hf, err := f.files("preview " + path)
	if err != nil {
		return nil, err
	}
	return hf.FilesPreview(ctx, path, w, h)
}

func (f *Fake) FilesTar(ctx context.Context, path string) (*FileRead, error) {
	hf, err := f.files("tar " + path)
	if err != nil {
		return nil, err
	}
	return hf.FilesTar(ctx, path)
}

func (f *Fake) FilesPut(ctx context.Context, dir, name, id string, off, size int64, body io.Reader) (*FilePut, error) {
	hf, err := f.files(fmt.Sprintf("put %s %s %d", dir, name, off))
	if err != nil {
		return nil, err
	}
	return hf.FilesPut(ctx, dir, name, id, off, size, body)
}

func (f *Fake) FilesHave(ctx context.Context, id string) (int64, error) {
	hf, err := f.files("have " + id)
	if err != nil {
		return 0, err
	}
	return hf.FilesHave(ctx, id)
}

func (f *Fake) FilesUntar(ctx context.Context, dir string, body io.Reader) (*FilePut, error) {
	hf, err := f.files("untar " + dir)
	if err != nil {
		return nil, err
	}
	return hf.FilesUntar(ctx, dir, body)
}

func (f *Fake) FilesOp(ctx context.Context, op FileOp) (*FileOpDone, error) {
	hf, err := f.files("op " + op.Op)
	if err != nil {
		return nil, err
	}
	return hf.FilesOp(ctx, op)
}

func (f *Fake) FilesRoute() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.HostFiles == nil {
		return RouteTailnet
	}
	return f.HostFiles.FilesRoute()
}
