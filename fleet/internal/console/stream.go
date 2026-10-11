package console

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// Cadences of the polled sources. A source is stale when its last success is
// older than twice its cadence.
const (
	CadenceStatusline = 5 * time.Second
	CadenceHost       = 2 * time.Second
	CadenceGit        = 10 * time.Second
	CadenceFleetd     = 5 * time.Second
	CadenceDocker     = 5 * time.Second // docker stats prints a frame every second or two
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
	started  time.Time // when the streamed process last came up
	last     Source    // OK/At of the last attempt's outcome; At is the last success
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

// start notes that a streamed process came up at `at`; it has produced
// nothing yet.
func (t *tracker) start(at time.Time) {
	t.mu.Lock()
	t.started = at
	t.last.OK, t.last.Error = true, "" // up, and not yet silent for long
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
		return !t.running || t.silentLocked(now) > 2*t.cadence && t.cadence > 0
	}
	return t.last.At.IsZero() || now.Sub(t.last.At) > 2*t.cadence
}

// silentLocked is how long a streamed source with a cadence has produced
// nothing parsed: since its last success, or since its process came up.
func (t *tracker) silentLocked(now time.Time) time.Duration {
	ref := t.last.At
	if t.started.After(ref) {
		ref = t.started
	}
	return now.Sub(ref)
}

// source is what a snapshot says: not ok when the last attempt failed or the
// source is stale.
func (t *tracker) source(now time.Time) Source {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.last
	if t.staleLocked(now) {
		s.OK = false
		if t.streamed && t.running && s.Error == "" {
			// the process runs but says nothing we can read
			s.Error = fmt.Sprintf("no stats parsed for %d s", int(t.silentLocked(now).Seconds()))
		}
	}
	return s
}

// spawner starts a long-lived process and returns its stdout. Closing the
// reader reaps the process. Tests replace it.
type spawner func(ctx context.Context, argv []string) (io.ReadCloser, error)

func execSpawn(ctx context.Context, argv []string) (io.ReadCloser, error) {
	cmd := target.HardenCtx(exec.CommandContext(ctx, argv[0], argv[1:]...))
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
	_ = target.KillGroup(p.cmd)
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
