package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/MelloB1989/karmax/fleet/internal/filexfer"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/hostfiles"
)

// Transfers: files between this machine and fleetd's, two at a time, each
// with its progress, speed and route. A file goes on from where a broken
// link left it, either way (a download from what it has, an upload from
// what fleetd holds), for as long as the link keeps coming back; a folder
// moves as one tar, and starts again.

// How a transfer keeps at it.
const (
	xferAtOnce   = 2
	xferAttempts = 30 // a file's tries, its backoff growing to xferBackMax
	xferBackMax  = 30 * time.Second
	dirAttempts  = 3
)

// States of a transfer.
const (
	xferQueued int32 = iota
	xferRunning
	xferDone
	xferFailed
	xferCancelled
)

type xfer struct {
	id   int
	up   bool   // this machine to fleetd's
	name string // what is moved, its base name
	src  string // its path where it is
	dst  string // the folder it goes into
	dir  bool
	open bool // a download to open here once done

	total atomic.Int64 // -1: not known
	done  atomic.Int64
	state atomic.Int32

	mu      sync.Mutex
	route   string
	err     error
	out     string // where it went
	note    string // a retry under way
	cancel  context.CancelFunc
	started time.Time
	ended   time.Time
	lastN   int64
	lastT   time.Time
	rate    float64 // bytes a second, smoothed
}

func (x *xfer) set(f func(x *xfer)) {
	x.mu.Lock()
	f(x)
	x.mu.Unlock()
}

// xferDoneMsg is a transfer ended, one way or another.
type xferDoneMsg struct{ id int }

// xferTickMsg moves the transfers' progress on.
type xferTickMsg struct{}

type xfers struct {
	mu   sync.Mutex
	list []*xfer
	next int
}

func (t *xfers) add(x *xfer) {
	t.mu.Lock()
	t.next++
	x.id = t.next
	x.total.Store(-1)
	t.list = append(t.list, x)
	t.mu.Unlock()
}

func (t *xfers) all() []*xfer {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*xfer(nil), t.list...)
}

func (t *xfers) byID(id int) *xfer {
	for _, x := range t.all() {
		if x.id == id {
			return x
		}
	}
	return nil
}

// active is how many are queued or running.
func (t *xfers) active() int {
	n := 0
	for _, x := range t.all() {
		if s := x.state.Load(); s == xferQueued || s == xferRunning {
			n++
		}
	}
	return n
}

// clear forgets the transfers that ended.
func (t *xfers) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	keep := t.list[:0]
	for _, x := range t.list {
		if s := x.state.Load(); s == xferQueued || s == xferRunning {
			keep = append(keep, x)
		}
	}
	t.list = keep
}

// pump starts the queued transfers there is room for.
func (t *xfers) pump(cl fleetapi.Client) tea.Cmd {
	running := 0
	var queued []*xfer
	for _, x := range t.all() {
		switch x.state.Load() {
		case xferRunning:
			running++
		case xferQueued:
			queued = append(queued, x)
		}
	}
	var cmds []tea.Cmd
	for _, x := range queued {
		if running >= xferAtOnce {
			break
		}
		running++
		ctx, cancel := context.WithCancel(context.Background())
		x.set(func(x *xfer) { x.cancel, x.started, x.lastT = cancel, time.Now(), time.Now() })
		x.state.Store(xferRunning)
		cmds = append(cmds, func() tea.Msg {
			defer cancel()
			err := runXfer(ctx, cl, x)
			x.set(func(x *xfer) { x.err, x.ended, x.note = err, time.Now(), "" })
			switch {
			case ctx.Err() != nil:
				x.state.Store(xferCancelled)
			case err != nil:
				x.state.Store(xferFailed)
			default:
				x.state.Store(xferDone)
				if x.open {
					openHere(x.out)
				}
			}
			return xferDoneMsg{x.id}
		})
	}
	return tea.Batch(cmds...)
}

// tick works out each running transfer's speed.
func (t *xfers) tick(now time.Time) {
	for _, x := range t.all() {
		if x.state.Load() != xferRunning {
			continue
		}
		x.set(func(x *xfer) {
			dt := now.Sub(x.lastT).Seconds()
			if dt < 0.4 {
				return
			}
			n := x.done.Load()
			inst := float64(n-x.lastN) / dt
			if x.rate == 0 {
				x.rate = inst
			} else {
				x.rate = 0.6*x.rate + 0.4*inst
			}
			x.lastN, x.lastT = n, now
		})
	}
}

func runXfer(ctx context.Context, cl fleetapi.Client, x *xfer) error {
	switch {
	case x.up && x.dir:
		return tries(ctx, x, dirAttempts, func() error { return upDir(ctx, cl, x) })
	case x.up:
		return upFile(ctx, cl, x)
	case x.dir:
		return tries(ctx, x, dirAttempts, func() error { return downDir(ctx, cl, x) })
	}
	return downFile(ctx, cl, x)
}

// permanent: an error that trying again cannot mend: fleetd refused, or
// the file is not there, or this machine's disk said no.
func permanent(err error) bool {
	var st interface{ Status() int }
	if errors.As(err, &st) {
		c := st.Status()
		return c >= 400 && c < 500 && c != 408 && c != 409 && c != 429
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return true
	}
	return errors.Is(err, hostfiles.ErrNotFound) || errors.Is(err, hostfiles.ErrInvalid) ||
		errors.Is(err, hostfiles.ErrDenied) || errors.Is(err, hostfiles.ErrTooLarge)
}

// tries runs f until it works, n times at most, waiting longer each time.
func tries(ctx context.Context, x *xfer, n int, f func() error) error {
	back := time.Second
	for i := 1; ; i++ {
		err := f()
		if err == nil || ctx.Err() != nil || permanent(err) || i >= n {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		x.set(func(x *xfer) { x.note = fmt.Sprintf("the link broke: trying again in %s (%d of %d)", back, i+1, n) })
		select {
		case <-time.After(back):
		case <-ctx.Done():
			return ctx.Err()
		}
		x.set(func(x *xfer) { x.note = "" })
		back = min(back*2, xferBackMax)
	}
}

// progress counts what passes into x.done, from base.
type progress struct {
	r io.Reader
	x *xfer
}

func (p progress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.x.done.Add(int64(n))
	return n, err
}

// downFile fetches a file into x.dst, resuming a part left by a broken
// link: the part is kept beside where the file goes, hidden.
func downFile(ctx context.Context, cl fleetapi.Client, x *xfer) error {
	if err := os.MkdirAll(x.dst, 0o755); err != nil {
		return err
	}
	part := filepath.Join(x.dst, fmt.Sprintf(".%s.fleetpart-%d-%d", x.name, os.Getpid(), x.id))
	defer os.Remove(part) // gone once renamed; a failure leaves nothing
	var mod time.Time
	return tries(ctx, x, xferAttempts, func() error {
		off := sizeOf(part)
		r, err := cl.FilesRead(ctx, x.src, off, -1, mod)
		if err != nil {
			return err
		}
		defer r.Body.Close()
		x.set(func(x *xfer) { x.route = r.Route })
		if r.From != off { // the file changed since: from the start
			if err := os.Truncate(part, r.From); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		mod = r.Mod
		x.total.Store(r.Size)
		x.done.Store(r.From)
		f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, progress{r.Body, x})
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if got := sizeOf(part); r.Size >= 0 && got < r.Size {
			return fmt.Errorf("the link broke at %s of %s", bytesOf(got), bytesOf(r.Size))
		}
		dest := hostfiles.Unique(x.dst, x.name)
		if err := os.Rename(part, dest); err != nil {
			return err
		}
		if !mod.IsZero() {
			_ = os.Chtimes(dest, mod, mod)
		}
		x.set(func(x *xfer) { x.out = dest })
		return nil
	})
}

func sizeOf(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

// downDir fetches a folder as a tar, unpacked beside where it goes, then
// moved there.
func downDir(ctx context.Context, cl fleetapi.Client, x *xfer) error {
	if err := os.MkdirAll(x.dst, 0o755); err != nil {
		return err
	}
	r, err := cl.FilesTar(ctx, x.src)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	x.set(func(x *xfer) { x.route = r.Route })
	x.total.Store(r.Size)
	x.done.Store(0)
	tmp, err := os.MkdirTemp(x.dst, ".fleet-get-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := filexfer.Extract(tmp, progress{r.Body, x}); err != nil {
		return err
	}
	tops, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, t := range tops {
		dest := hostfiles.Unique(x.dst, t.Name())
		if err := os.Rename(filepath.Join(tmp, t.Name()), dest); err != nil {
			return err
		}
		x.set(func(x *xfer) {
			if x.out == "" {
				x.out = dest
			}
		})
	}
	return nil
}

// upFile sends a file to x.dst on fleetd's machine; a broken link goes on
// from what fleetd holds of it.
func upFile(ctx context.Context, cl fleetapi.Client, x *xfer) error {
	f, err := os.Open(x.src)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	x.total.Store(size)
	id := hostfiles.NewUploadID()
	first := true
	return tries(ctx, x, xferAttempts, func() error {
		have := int64(0)
		if !first {
			if have, err = cl.FilesHave(ctx, id); err != nil {
				return err
			}
		}
		first = false
		if _, err := f.Seek(have, io.SeekStart); err != nil {
			return err
		}
		x.done.Store(have)
		x.set(func(x *xfer) { x.route = cl.FilesRoute() })
		p, err := cl.FilesPut(ctx, x.dst, x.name, id, have, size, progress{io.LimitReader(f, size-have), x})
		if err != nil {
			return err
		}
		if !p.Done {
			return fmt.Errorf("fleetd holds %s of %s", bytesOf(p.Have), bytesOf(size))
		}
		x.set(func(x *xfer) { x.out = p.Path })
		return nil
	})
}

// upDir sends a folder as a tar, unpacked on fleetd's machine.
func upDir(ctx context.Context, cl fleetapi.Client, x *xfer) error {
	if n, err := dirSize(x.src); err == nil {
		x.total.Store(n)
	}
	x.done.Store(0)
	pr, pw := io.Pipe()
	var n atomic.Int64
	go func() {
		stop := make(chan struct{})
		go func() { // the bytes packed are the progress
			t := time.NewTicker(100 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					x.done.Store(n.Load())
				case <-stop:
					return
				}
			}
		}()
		_, err := hostfiles.Pack(ctx, pw, x.src, &n)
		close(stop)
		x.done.Store(n.Load())
		pw.CloseWithError(err)
	}()
	defer pr.Close()
	x.set(func(x *xfer) { x.route = cl.FilesRoute() })
	p, err := cl.FilesUntar(ctx, x.dst, pr)
	if err != nil {
		return err
	}
	x.set(func(x *xfer) { x.out = p.Path })
	return nil
}

// openHere opens a file with this machine's own app for it.
func openHere(p string) {
	if p == "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", p)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", p)
	default:
		cmd = exec.Command("xdg-open", p)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

// openDir is where a file fetched to be opened goes.
func openDir() string {
	return filepath.Join(os.TempDir(), "fleetctl-open")
}
