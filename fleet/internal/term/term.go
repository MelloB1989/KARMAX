// Package term is fleetd's side of a terminal session (fleetctl ssh): a
// command under a pty, its bytes pumped to and from a websocket, and the one
// typist per worker. The route that runs it is server/term.go.
//
// Terminal bytes are binary frames both ways; control frames are
// fleetapi.TermFrame JSON as text frames.
package term

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// Marker is the environment variable every session's process carries in the
// container, with the session's id: what Cleanup finds it by. Killing the
// docker exec on fleetd's side does not end what it started in there.
const Marker = "FLEET_TERM"

// Target is where a worker's session lives: its container, reached with its
// host's exec prefix, and the tmux session its claude runs in.
type Target struct {
	Worker    string
	Host      string
	Container target.Container
	Pane      string // main, main-2
}

// Argv is the command a mode runs, per the spec's table: watch attaches
// read-only and never sizes the window; type attaches; shell is a login
// shell in /work.
func Argv(t Target, mode fleetapi.TermMode, id string) []string {
	argv := append([]string{}, t.Container.Exec...)
	argv = append(argv, "exec", "-it", "-e", Marker+"="+id)
	switch mode {
	case fleetapi.TermShell:
		return append(argv, "-w", "/work", t.Container.Name, "bash", "-l")
	case fleetapi.TermType:
		return append(argv, t.Container.Name, "tmux", "attach", "-t", "="+t.Pane)
	}
	return append(argv, t.Container.Name, "tmux", "attach", "-t", "="+t.Pane, "-f", "read-only,ignore-size")
}

// cleanupScript ends every process in the container that carries the
// session's marker ($1): the tmux client, or the shell and its children.
const cleanupScript = `for d in /proc/[0-9]*; do
	p=${d#/proc/}
	[ "$p" = "$$" ] && continue
	tr '\0' '\n' < "$d/environ" 2>/dev/null | grep -qx "` + Marker + `=$1" && kill -HUP "$p" 2>/dev/null
done
exit 0`

// Cleanup ends what a session killed on fleetd's side left in the container.
// Best effort: a container that is gone took them with it.
func Cleanup(t Target, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Container.Sh(ctx, cleanupScript, id)
}

// StartFunc starts argv under a pty of the given size and returns the
// master side and the process.
type StartFunc func(argv []string, cols, rows int) (*os.File, *exec.Cmd, error)

// Start is the real StartFunc.
func Start(argv []string, cols, rows int) (*os.File, *exec.Cmd, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	return f, cmd, err
}

// ---- the socket ----

type msg struct {
	typ websocket.MessageType
	b   []byte
}

// Conn is a websocket that sessions run on in turn: a typist released by an
// admin goes on watching on the same socket. One reader runs for its whole
// life, so that ending a session never cancels a read (which would close
// the socket).
type Conn struct {
	ws     *websocket.Conn
	in     chan msg
	closed chan struct{} // the client went away
	wmu    sync.Mutex
}

// Ping cadence and the wait for the pong. A client that vanished without a
// FIN (a closed laptop) would otherwise hold the keyboard until TCP gave up.
var (
	PingEvery   = 30 * time.Second
	PingTimeout = 10 * time.Second
)

// NewConn starts reading ws until it closes or ctx ends.
func NewConn(ctx context.Context, ws *websocket.Conn) *Conn {
	ws.SetReadLimit(1 << 20)
	c := &Conn{ws: ws, in: make(chan msg, 64), closed: make(chan struct{})}
	go func() {
		defer close(c.closed)
		for {
			typ, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			select {
			case c.in <- msg{typ, b}:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { // pings; an unanswered one drops the client
		tk := time.NewTicker(PingEvery)
		defer tk.Stop()
		for {
			select {
			case <-c.closed:
				return
			case <-tk.C:
				pctx, cancel := context.WithTimeout(ctx, PingTimeout)
				err := ws.Ping(pctx)
				cancel()
				if err != nil {
					ws.CloseNow()
					return
				}
			}
		}
	}()
	return c
}

// Frame sends a control frame.
func (c *Conn) Frame(f fleetapi.TermFrame) error {
	b, _ := json.Marshal(f)
	return c.write(websocket.MessageText, b)
}

func (c *Conn) write(typ websocket.MessageType, b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.ws.Write(ctx, typ, b)
}

// Close ends the socket normally.
func (c *Conn) Close() {
	c.ws.Close(websocket.StatusNormalClosure, "")
}

// ---- a session ----

// Session is one command under a pty, joined to a Conn.
type Session struct {
	Argv       []string
	Cols, Rows int
	// Input lets keystrokes reach the command. A watcher's are dropped.
	Input bool
	Start StartFunc // nil: Start
	// Hello is sent once the command runs, with the size filled in; no
	// hello is sent when its Type is empty.
	Hello fleetapi.TermFrame
	// Resized is called after the pty takes a size the client sent: a
	// typist sizes the tmux window to its terminal.
	Resized func(cols, rows int)
	// Idle ends the session when the client sends nothing for this long; 0
	// never does.
	Idle time.Duration
	// Released, when it closes, ends the session: an admin took the
	// keyboard.
	Released <-chan struct{}
}

// Result is how a session ended.
type Result struct {
	// Killed is fleetd ending the command (the client left, idled, or was
	// released) rather than the command ending by itself: what it started
	// in the container may still run (Cleanup).
	Killed   bool
	Released bool
	Gone     bool  // the client went away
	Err      error // the command could not start, or exited with an error
}

// Run runs the session until the command exits, the client goes, the
// session idles out, it is released, or ctx ends. An exit or error frame
// says why when the client is still there to read it.
func (s *Session) Run(ctx context.Context, c *Conn) Result {
	start := s.Start
	if start == nil {
		start = Start
	}
	f, cmd, err := start(s.Argv, s.Cols, s.Rows)
	if err != nil {
		err = fmt.Errorf("start the session: %w", err)
		c.Frame(fleetapi.TermFrame{Type: fleetapi.FrameError, Error: err.Error()})
		return Result{Err: err}
	}
	if s.Hello.Type != "" {
		h := s.Hello
		h.Cols, h.Rows = s.Cols, s.Rows
		c.Frame(h)
	}

	var tailMu sync.Mutex
	var tail []byte // the last output, for an error frame's reason
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				tailMu.Lock()
				tail = append(tail, buf[:n]...)
				if len(tail) > 1024 {
					tail = tail[len(tail)-1024:]
				}
				tailMu.Unlock()
				if c.write(websocket.MessageBinary, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var idle <-chan time.Time
	var timer *time.Timer
	if s.Idle > 0 {
		timer = time.NewTimer(s.Idle)
		defer timer.Stop()
		idle = timer.C
	}
	// kill ends the command and waits for it and its output.
	kill := func(r Result) Result {
		target.KillGroup(cmd)
		<-waited
		f.Close()
		<-outDone
		r.Killed = true
		return r
	}
	for {
		select {
		case <-ctx.Done():
			return kill(Result{Gone: true})
		case <-c.closed:
			return kill(Result{Gone: true})
		case <-s.Released:
			return kill(Result{Released: true})
		case <-idle:
			r := kill(Result{})
			c.Frame(fleetapi.TermFrame{Type: fleetapi.FrameExit, Error: fmt.Sprintf("closed after %s with nothing from you", s.Idle)})
			return r
		case werr := <-waited:
			// The pty may be held open by something the command left; its
			// output is what is buffered now.
			select {
			case <-outDone:
			case <-time.After(time.Second):
			}
			f.Close()
			<-outDone
			if werr != nil {
				tailMu.Lock()
				why := lastLine(string(tail))
				tailMu.Unlock()
				if why == "" {
					why = werr.Error()
				}
				c.Frame(fleetapi.TermFrame{Type: fleetapi.FrameError, Error: why})
				return Result{Err: errors.New(why)}
			}
			c.Frame(fleetapi.TermFrame{Type: fleetapi.FrameExit})
			return Result{}
		case m := <-c.in:
			if timer != nil {
				timer.Reset(s.Idle)
			}
			if m.typ == websocket.MessageBinary {
				if s.Input {
					f.Write(m.b)
				}
				continue
			}
			var fr fleetapi.TermFrame
			if json.Unmarshal(m.b, &fr) != nil || fr.Type != fleetapi.FrameResize || fr.Cols <= 0 || fr.Rows <= 0 {
				continue
			}
			fr.Cols, fr.Rows = min(fr.Cols, 1000), min(fr.Rows, 1000)
			if pty.Setsize(f, &pty.Winsize{Cols: uint16(fr.Cols), Rows: uint16(fr.Rows)}) == nil && s.Resized != nil {
				s.Resized(fr.Cols, fr.Rows)
			}
		}
	}
}

// lastLine is the last non-blank line of a terminal's output.
func lastLine(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r", ""), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// ---- the keyboard ----

// ErrHeld is a type session asked for while someone else types.
type ErrHeld struct{ Holder string }

func (e ErrHeld) Error() string { return "held by " + e.Holder }

// Locks is one typist per worker. The zero value is ready.
type Locks struct {
	mu   sync.Mutex
	held map[string]*Lease
}

// Lease is a worker's keyboard, held until Release.
type Lease struct {
	Holder   string
	worker   string
	l        *Locks
	released chan struct{}
	once     sync.Once
}

// Take gives who worker's keyboard. When someone has it, it is ErrHeld,
// unless force: then the holder's lease is released (Released closes) and
// the keyboard is who's.
func (l *Locks) Take(worker, who string, force bool) (*Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = map[string]*Lease{}
	}
	if h, ok := l.held[worker]; ok {
		if !force {
			return nil, ErrHeld{Holder: h.Holder}
		}
		h.once.Do(func() { close(h.released) })
	}
	le := &Lease{Holder: who, worker: worker, l: l, released: make(chan struct{})}
	l.held[worker] = le
	return le, nil
}

// Holder is who types in worker, or "".
func (l *Locks) Holder(worker string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if h, ok := l.held[worker]; ok {
		return h.Holder
	}
	return ""
}

// Released closes when an admin takes the keyboard from this lease.
func (le *Lease) Released() <-chan struct{} { return le.released }

// Release gives the keyboard back. A lease taken over frees nothing of the
// new holder's.
func (le *Lease) Release() {
	le.l.mu.Lock()
	defer le.l.mu.Unlock()
	if le.l.held[le.worker] == le {
		delete(le.l.held, le.worker)
	}
}
