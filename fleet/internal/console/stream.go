package console

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Cadences of the polled sources. A source is stale when its last success is
// older than twice its cadence.
const (
	CadenceStatusline = 5 * time.Second
	CadenceHost       = 2 * time.Second
	CadenceGit        = 10 * time.Second
	CadenceFleetd     = 5 * time.Second
	cadenceActivity   = 60 * time.Second
	cadenceToday      = 30 * time.Second
)

// Backoff for a streamed source's restarts: 1 s, doubling, capped at 30 s.
const (
	backoffMin = time.Second
	backoffMax = 30 * time.Second
)

// tracker is a source's health: the last success or failure, and for a
// streamed source whether its process is running.
type tracker struct {
	mu       sync.Mutex
	cadence  time.Duration
	streamed bool
	running  bool
	last     Source // OK/At of the last attempt's outcome; At is the last success
}

func newTracker(cadence time.Duration, streamed bool) *tracker {
	return &tracker{cadence: cadence, streamed: streamed}
}

func (t *tracker) success(at time.Time) {
	t.mu.Lock()
	t.last = Source{OK: true, At: at}
	t.mu.Unlock()
}

func (t *tracker) failure(err error) {
	t.mu.Lock()
	t.last.OK = false
	t.last.Error = err.Error()
	t.mu.Unlock()
}

func (t *tracker) setRunning(r bool) {
	t.mu.Lock()
	t.running = r
	t.mu.Unlock()
}

// stale: a polled source's last success is older than twice its cadence (or
// there never was one); a streamed source's process is not running.
func (t *tracker) stale(now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.staleLocked(now)
}

func (t *tracker) staleLocked(now time.Time) bool {
	if t.streamed {
		return !t.running
	}
	return t.last.At.IsZero() || now.Sub(t.last.At) > 2*t.cadence
}

// source is what a snapshot says: not ok when the last attempt failed or the
// source is stale.
func (t *tracker) source(now time.Time) Source {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.last
	if t.staleLocked(now) {
		s.OK = false
	}
	return s
}

// spawner starts a long-lived process and returns its stdout. Closing the
// reader reaps the process. Tests replace it.
type spawner func(ctx context.Context, argv []string) (io.ReadCloser, error)

func execSpawn(ctx context.Context, argv []string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &procReader{out: out, cmd: cmd}, nil
}

type procReader struct {
	out io.ReadCloser
	cmd *exec.Cmd
}

func (p *procReader) Read(b []byte) (int, error) { return p.out.Read(b) }
func (p *procReader) Close() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return p.cmd.Wait()
}

// streamLines runs argv through sp and calls onLine for each line until the
// process ends or ctx is done, restarting with backoff. tr.running follows
// the process. onStart runs each time the process is up.
func streamLines(ctx context.Context, sp spawner, argv []string, tr *tracker, now func() time.Time, onStart func(), onLine func([]byte)) {
	wait := backoffMin
	for ctx.Err() == nil {
		started := now()
		err := streamOnce(ctx, sp, argv, tr, onStart, onLine)
		tr.setRunning(false)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		tr.failure(err)
		if now().Sub(started) > backoffMax {
			wait = backoffMin // it ran for a while: not a crash loop
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, backoffMax)
	}
}

func streamOnce(ctx context.Context, sp spawner, argv []string, tr *tracker, onStart func(), onLine func([]byte)) error {
	rc, err := sp(ctx, argv)
	if err != nil {
		return err
	}
	defer rc.Close()
	tr.setRunning(true)
	if onStart != nil {
		onStart()
	}
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		onLine(sc.Bytes())
	}
	return sc.Err()
}

// every runs fn now and then each period until ctx is done.
func every(ctx context.Context, period time.Duration, fn func(context.Context)) {
	fn(ctx)
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}
