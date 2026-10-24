package console

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"

	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// StartFunc starts argv under a PTY and returns the master side and the
// process. The default sizes the PTY to the configured terminal.
type StartFunc func(argv []string) (*os.File, *exec.Cmd, error)

// Errors from Watch and Control.
var (
	ErrUnknownAgent = errors.New("no such live agent")
	ErrSlow         = errors.New("subscriber too slow; resubscribe")
	ErrEnded        = errors.New("terminal ended")
)

// ErrHeld is a second Control request for an agent that has a controller.
type ErrHeld struct{ Holder string }

func (e ErrHeld) Error() string { return "held by " + e.Holder }

const (
	watchIdleClose = 30 * time.Second
	subQueue       = 512
)

// Terms is the terminals: one shared read-only tmux client per agent for
// everyone watching, and at most one read-write controller.
type Terms struct {
	cfg   *Config
	start StartFunc
	// Runner runs the host's tmux queries (list-clients, refresh-client).
	Runner target.Runner

	idleClose time.Duration
	// Set at construction, read-only after: the term route's ping cadence.
	pingEvery, pingTimeout time.Duration
	backoff                time.Duration

	mu      sync.Mutex
	watches map[string]*termWatch
	holders map[string]string
	lastAt  time.Time
	lastErr string
}

func NewTerms(cfg *Config, start StartFunc) *Terms {
	if start == nil {
		size := cfg.Term
		start = func(argv []string) (*os.File, *exec.Cmd, error) {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Env = append(os.Environ(), "TERM=xterm-256color")
			f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(size.Rows), Cols: uint16(size.Cols)})
			return f, cmd, err
		}
	}
	return &Terms{cfg: cfg, start: start, Runner: target.Exec{}, idleClose: watchIdleClose, pingEvery: defaultPingEvery, pingTimeout: defaultPingTimeout, backoff: backoffMin,
		watches: map[string]*termWatch{}, holders: map[string]string{}}
}

func (t *Terms) conf(agent string) (AgentConf, bool) {
	for _, a := range t.cfg.Agents {
		if a.Name == agent && a.Live {
			return a, true
		}
	}
	return AgentConf{}, false
}

func (t *Terms) container(c AgentConf) target.Container {
	return target.Container{Exec: c.Exec, Name: c.Container, Runner: t.Runner}
}

// argv is the attach command, run with the stream marker id so the client
// can be found, and detached, from inside the container.
func (t *Terms) argv(c AgentConf, readonly bool, id string) []string {
	cmd := []string{"tmux", "attach", "-t", c.pane()}
	if readonly {
		cmd = []string{"tmux", "attach", "-r", "-t", c.pane()}
	}
	return t.container(c).InteractiveArgv(withStreamEnv(id, cmd...)...)
}

// Holder is who controls agent now, or "".
func (t *Terms) Holder(agent string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.holders[agent]
}

// Source is the terminal source's health for the snapshot: ok unless a
// client is down and being restarted.
func (t *Terms) Source() Source {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Source{OK: t.lastErr == "", At: t.lastAt, Error: t.lastErr}
}

func (t *Terms) note(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastAt = time.Now()
	if err != nil {
		t.lastErr = err.Error()
	} else {
		t.lastErr = ""
	}
}

// ---- watch ----

type item struct {
	b  []byte
	fr *TermFrame
}

type sub struct{ ch chan item }

type termWatch struct {
	t     *Terms
	agent string
	conf  AgentConf

	mu    sync.Mutex
	subs  map[*sub]struct{}
	cmd   *exec.Cmd
	id    string // the stream marker of cmd's client
	stop  chan struct{}
	timer *time.Timer
	dead  bool
}

// Watch streams agent's terminal to out until ctx is done. The first call
// opens the shared client, and every later one makes tmux redraw the whole
// screen, so a viewer never starts on a half-drawn one. frame carries the
// JSON frames (hello, reconnecting).
func (t *Terms) Watch(ctx context.Context, agent string, out func([]byte), frame func(TermFrame)) error {
	conf, ok := t.conf(agent)
	if !ok {
		return ErrUnknownAgent
	}
	s := &sub{ch: make(chan item, subQueue)}
	w, err := t.join(conf, s)
	if err != nil {
		t.note(err)
		return err
	}
	defer w.leave(s)
	frame(TermFrame{Type: "hello", Agent: agent, Mode: "watch", Cols: t.cfg.Term.Cols, Rows: t.cfg.Term.Rows})
	go w.redraw()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case it, ok := <-s.ch:
			if !ok {
				if w.isDead() {
					return ErrEnded
				}
				return ErrSlow
			}
			if it.fr != nil {
				frame(*it.fr)
			} else {
				out(it.b)
			}
		}
	}
}

// Redraw makes tmux repaint agent's shared screen, for a viewer that joined
// the socket after the first paint. It does nothing when nobody watches.
func (t *Terms) Redraw(agent string) {
	t.mu.Lock()
	w := t.watches[agent]
	t.mu.Unlock()
	if w != nil {
		go w.redraw()
	}
}

func (t *Terms) join(conf AgentConf, s *sub) (*termWatch, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.watches[conf.Name]
	if w == nil {
		w = &termWatch{t: t, agent: conf.Name, conf: conf, subs: map[*sub]struct{}{}, stop: make(chan struct{})}
		id := newStreamID()
		f, cmd, err := t.start(t.argv(conf, true, id))
		if err != nil {
			return nil, fmt.Errorf("watch %s: %w", conf.Name, err)
		}
		w.cmd, w.id = cmd, id
		t.watches[conf.Name] = w
		go w.run(f, cmd, id)
	}
	w.mu.Lock()
	w.subs[s] = struct{}{}
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.mu.Unlock()
	return w, nil
}

func (w *termWatch) isDead() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dead
}

func (w *termWatch) leave(s *sub) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.subs, s)
	if len(w.subs) == 0 && w.timer == nil && !w.dead {
		w.timer = time.AfterFunc(w.t.idleClose, w.idleExpired)
	}
}

// idleExpired closes the client if nobody came back.
func (w *termWatch) idleExpired() {
	w.t.mu.Lock()
	w.mu.Lock()
	if len(w.subs) > 0 || w.dead {
		w.mu.Unlock()
		w.t.mu.Unlock()
		return
	}
	w.dead = true
	close(w.stop)
	cmd, id := w.cmd, w.id
	delete(w.t.watches, w.agent)
	w.mu.Unlock()
	w.t.mu.Unlock()
	if cmd != nil {
		target.KillGroup(cmd)
		go detachClients(w.t.container(w.conf), id) // the client may outlive its docker exec
	}
}

func (w *termWatch) broadcast(it item) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for s := range w.subs {
		select {
		case s.ch <- it:
		default: // dropping bytes would leave a half-drawn screen: drop the viewer, who reconnects
			delete(w.subs, s)
			close(s.ch)
		}
	}
}

// run reads the shared client until it ends, then restarts it with backoff
// while anyone is still watching.
func (w *termWatch) run(f *os.File, cmd *exec.Cmd, id string) {
	wait := w.t.backoff
	for {
		started := time.Now()
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				w.broadcast(item{b: append([]byte(nil), buf[:n]...)})
				w.t.note(nil)
			}
			if err != nil {
				break
			}
		}
		f.Close()
		cmd.Wait()
		detachClients(w.t.container(w.conf), id)

		w.mu.Lock()
		if w.dead || len(w.subs) == 0 {
			w.dead = true
			for s := range w.subs {
				close(s.ch)
			}
			w.subs = map[*sub]struct{}{}
			select {
			case <-w.stop:
			default:
				close(w.stop)
			}
			w.mu.Unlock()
			w.t.mu.Lock()
			if w.t.watches[w.agent] == w {
				delete(w.t.watches, w.agent)
			}
			w.t.mu.Unlock()
			return
		}
		w.mu.Unlock()
		w.t.note(fmt.Errorf("%s: terminal client exited", w.agent))
		w.broadcast(item{fr: &TermFrame{Type: "reconnecting", Agent: w.agent, Mode: "watch"}})
		if time.Since(started) > backoffMax {
			wait = w.t.backoff
		}
		for {
			select {
			case <-w.stop:
				return
			case <-time.After(wait):
			}
			wait = min(wait*2, backoffMax)
			var err error
			id = newStreamID()
			f, cmd, err = w.t.start(w.t.argv(w.conf, true, id))
			if err == nil {
				w.mu.Lock()
				w.cmd, w.id = cmd, id
				w.mu.Unlock()
				break
			}
			w.t.note(err)
		}
	}
}

// redraw makes tmux repaint the read-only client's whole screen. There is
// one read-only client per worker's pane (ours), so the flag finds it; the client
// pid is not comparable through docker exec.
func (w *termWatch) redraw() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := w.t.container(w.conf)
	for range 10 { // the client may still be attaching
		out, err := c.Run(ctx, "tmux", "list-clients", "-t", w.conf.pane(), "-F", "#{client_tty} #{client_readonly}")
		if err == nil {
			found := false
			for _, l := range strings.Split(string(out), "\n") {
				if tty, ro, ok := strings.Cut(strings.TrimSpace(l), " "); ok && ro == "1" {
					found = true
					c.Run(ctx, "tmux", "refresh-client", "-t", tty)
				}
			}
			if found {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ---- control ----

// acquire takes agent's one controller slot.
func (t *Terms) acquire(agent, who string) (release func(), err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if h, ok := t.holders[agent]; ok {
		return nil, ErrHeld{Holder: h}
	}
	t.holders[agent] = who
	return func() {
		t.mu.Lock()
		delete(t.holders, agent)
		t.mu.Unlock()
	}, nil
}

// Control gives who agent's keyboard: in carries keystrokes, out gets the
// screen. There is one controller per agent; a second gets ErrHeld. The
// lock is released only once the client process is gone, when ctx ends, in
// closes, the process exits or Ctrl+] detaches it (a "released" frame).
func (t *Terms) Control(ctx context.Context, agent, who string, in <-chan []byte, out func([]byte), frame func(TermFrame)) error {
	conf, ok := t.conf(agent)
	if !ok {
		return ErrUnknownAgent
	}
	release, err := t.acquire(agent, who)
	if err != nil {
		return err
	}
	return t.control(ctx, conf, release, in, out, frame)
}

func (t *Terms) control(ctx context.Context, conf AgentConf, release func(), in <-chan []byte, out func([]byte), frame func(TermFrame)) error {
	defer release()
	id := newStreamID()
	f, cmd, err := t.start(t.argv(conf, false, id))
	if err != nil {
		return fmt.Errorf("control %s: %w", conf.Name, err)
	}
	var once sync.Once
	reap := func() { // after this the client is gone and the lock may go
		once.Do(func() {
			target.KillGroup(cmd)
			f.Close()
			cmd.Wait()
			// Killing the host-side docker exec does not end the tmux client
			// in the container: detach it, or it stays read-write.
			detachClients(t.container(conf), id)
		})
	}
	defer reap()
	frame(TermFrame{Type: "hello", Agent: conf.Name, Mode: "control", Cols: t.cfg.Term.Cols, Rows: t.cfg.Term.Rows})

	done := make(chan struct{})
	go func() { // the PTY's output; ends when the client does
		defer close(done)
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				out(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			frame(TermFrame{Type: "released", Agent: conf.Name, Mode: "control"})
			return nil
		case b, ok := <-in:
			if !ok {
				return nil
			}
			if _, err := f.Write(b); err != nil {
				<-done
				frame(TermFrame{Type: "released", Agent: conf.Name, Mode: "control"})
				return nil
			}
		}
	}
}
