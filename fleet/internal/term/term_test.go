package term

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// client is the test's end of a socket: what fleetd sent, as it arrives.
type client struct {
	t  *testing.T
	c  *websocket.Conn
	mu sync.Mutex
	b  strings.Builder
	fr []fleetapi.TermFrame
	up chan struct{} // a nudge on every message
}

func (c *client) read() {
	for {
		typ, b, err := c.c.Read(context.Background())
		if err != nil {
			close(c.up)
			return
		}
		c.mu.Lock()
		if typ == websocket.MessageBinary {
			c.b.Write(b)
		} else {
			var f fleetapi.TermFrame
			json.Unmarshal(b, &f)
			c.fr = append(c.fr, f)
		}
		c.mu.Unlock()
		select {
		case c.up <- struct{}{}:
		default:
		}
	}
}

// until waits for ok, re-checked on every message, for at most 10 s.
func (c *client) until(what string, ok func(out string, frames []fleetapi.TermFrame) bool) {
	c.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		c.mu.Lock()
		done := ok(c.b.String(), slices.Clone(c.fr))
		out := c.b.String()
		c.mu.Unlock()
		if done {
			return
		}
		select {
		case _, open := <-c.up:
			if !open {
				c.mu.Lock()
				done := ok(c.b.String(), slices.Clone(c.fr))
				c.mu.Unlock()
				if !done {
					c.t.Fatalf("socket closed before %s; output %q, frames %+v", what, out, c.fr)
				}
				return
			}
		case <-deadline:
			c.t.Fatalf("no %s in 10s; output %q, frames %+v", what, out, c.fr)
		}
	}
}

func (c *client) send(b []byte) {
	if err := c.c.Write(context.Background(), websocket.MessageBinary, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) frame(f fleetapi.TermFrame) {
	b, _ := json.Marshal(f)
	if err := c.c.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
}

func hasFrame(typ string) func(string, []fleetapi.TermFrame) bool {
	return func(_ string, fr []fleetapi.TermFrame) bool {
		return slices.ContainsFunc(fr, func(f fleetapi.TermFrame) bool { return f.Type == typ })
	}
}

func contains(s string) func(string, []fleetapi.TermFrame) bool {
	return func(out string, _ []fleetapi.TermFrame) bool { return strings.Contains(out, s) }
}

// serve runs the session sess(conn) for every socket, and returns a dial and
// the session's result.
func serve(t *testing.T, sess func(context.Context, *Conn) Result) (dial func() *client, results chan Result) {
	t.Helper()
	results = make(chan Result, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn := NewConn(r.Context(), ws)
		res := sess(r.Context(), conn)
		conn.Close()
		results <- res
	}))
	t.Cleanup(srv.Close)
	return func() *client {
		ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		ws.SetReadLimit(1 << 20)
		c := &client{t: t, c: ws, up: make(chan struct{}, 1)}
		go c.read()
		t.Cleanup(func() { ws.CloseNow() })
		return c
	}, results
}

// echo is the fake exec: a shell under the pty that prints its size, then
// echoes what it reads. pidfile, when set, records its pid.
func echo(pidfile string) []string {
	script := "stty size; cat"
	if pidfile != "" {
		script = "echo $$ > " + pidfile + "; stty size; exec cat"
	}
	return []string{"/bin/sh", "-c", script}
}

func result(t *testing.T, results chan Result) Result {
	t.Helper()
	select {
	case r := <-results:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not end")
	}
	return Result{}
}

func TestWatchIgnoresInput(t *testing.T) {
	tg := Target{Worker: "agent-03:2", Container: target.Container{Exec: []string{"docker", "--context", "pc2"}, Name: "fleet-agent-03"}, Pane: "main-2"}
	argv := Argv(tg, fleetapi.TermWatch, "id1")
	got := strings.Join(argv, " ")
	if !strings.Contains(got, "tmux attach -t =main-2 -f read-only,ignore-size") {
		t.Fatalf("watch argv = %q, want a read-only, ignore-size attach", got)
	}

	resized := make(chan struct{}, 1)
	dial, results := serve(t, func(ctx context.Context, c *Conn) Result {
		s := &Session{Argv: echo(""), Cols: 80, Rows: 24, Input: false,
			Resized: func(int, int) { resized <- struct{}{} }}
		return s.Run(ctx, c)
	})
	c := dial()
	c.until("the size", contains("24 80"))
	c.send([]byte("typed-by-a-watcher\n"))
	c.frame(fleetapi.TermFrame{Type: fleetapi.FrameResize, Cols: 100, Rows: 30})
	<-resized // the keystrokes before it were handled, and dropped
	c.c.Close(websocket.StatusNormalClosure, "")
	result(t, results)
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.Contains(c.b.String(), "typed-by-a-watcher") {
		t.Fatalf("a watcher's keystrokes reached the session: %q", c.b.String())
	}
}

func TestTypeEchoes(t *testing.T) {
	dial, _ := serve(t, func(ctx context.Context, c *Conn) Result {
		s := &Session{Argv: echo(""), Cols: 80, Rows: 24, Input: true,
			Hello: fleetapi.TermFrame{Type: fleetapi.FrameHello, Worker: "agent-03", Mode: fleetapi.TermType}}
		return s.Run(ctx, c)
	})
	c := dial()
	c.until("hello", hasFrame(fleetapi.FrameHello))
	c.send([]byte("hi there\n"))
	c.until("the echo", func(out string, _ []fleetapi.TermFrame) bool { return strings.Count(out, "hi there") >= 2 })
	c.mu.Lock()
	h := c.fr[0]
	c.mu.Unlock()
	if h.Cols != 80 || h.Rows != 24 || h.Worker != "agent-03" {
		t.Fatalf("hello = %+v, want the worker and the size", h)
	}
}

func TestASecondTypistIsHeld(t *testing.T) {
	var l Locks
	first, err := l.Take("agent-03", "ram", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.Take("agent-03", "friend@example.com", false)
	var held ErrHeld
	if !errors.As(err, &held) || held.Holder != "ram" {
		t.Fatalf("second Take = %v, want held by ram", err)
	}
	if _, err := l.Take("agent-03:2", "friend@example.com", false); err != nil {
		t.Fatalf("another slot is its own worker: %v", err)
	}
	first.Release()
	if _, err := l.Take("agent-03", "friend@example.com", false); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestAForcedTakeoverReleasesTheHolder(t *testing.T) {
	var l Locks
	first, _ := l.Take("agent-03", "friend@example.com", false)
	second, err := l.Take("agent-03", "operator", true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.Released():
	default:
		t.Fatal("the holder was not told it lost the keyboard")
	}
	first.Release() // the old holder leaving frees nothing of the new one's
	if _, err := l.Take("agent-03", "ram", false); err == nil {
		t.Fatal("the old holder's release freed the new holder's keyboard")
	}
	second.Release()

	// A session whose lease is taken ends as released.
	var mu sync.Mutex
	var lease *Lease
	dial, results := serve(t, func(ctx context.Context, c *Conn) Result {
		mu.Lock()
		lease, _ = l.Take("agent-03", "friend@example.com", false)
		mu.Unlock()
		s := &Session{Argv: echo(""), Cols: 80, Rows: 24, Input: true, Released: lease.Released()}
		return s.Run(ctx, c)
	})
	c := dial()
	c.until("the size", contains("24 80"))
	if _, err := l.Take("agent-03", "operator", true); err != nil {
		t.Fatal(err)
	}
	if r := result(t, results); !r.Released {
		t.Fatalf("result = %+v, want released", r)
	}
}

func TestResizeReachesThePty(t *testing.T) {
	resized := make(chan [2]int, 1)
	dial, _ := serve(t, func(ctx context.Context, c *Conn) Result {
		s := &Session{Argv: []string{"/bin/sh", "-c", "stty size; read x; stty size; cat"}, Cols: 80, Rows: 24, Input: true,
			Resized: func(cols, rows int) { resized <- [2]int{cols, rows} }}
		return s.Run(ctx, c)
	})
	c := dial()
	c.until("the first size", contains("24 80"))
	c.frame(fleetapi.TermFrame{Type: fleetapi.FrameResize, Cols: 132, Rows: 43})
	if got := <-resized; got != [2]int{132, 43} {
		t.Fatalf("resized to %v", got)
	}
	c.send([]byte("\n"))
	c.until("the new size", contains("43 132"))
}

func TestTheSessionEndsWhenTheProcessExits(t *testing.T) {
	dial, results := serve(t, func(ctx context.Context, c *Conn) Result {
		s := &Session{Argv: []string{"/bin/sh", "-c", "echo bye"}, Cols: 80, Rows: 24, Input: true}
		return s.Run(ctx, c)
	})
	c := dial()
	c.until("an exit frame", hasFrame(fleetapi.FrameExit))
	if r := result(t, results); r.Killed || r.Err != nil {
		t.Fatalf("result = %+v, want a clean exit", r)
	}

	// A command that fails says why: the container is gone.
	dial, _ = serve(t, func(ctx context.Context, c *Conn) Result {
		s := &Session{Argv: []string{"/bin/sh", "-c", "echo 'Error response from daemon: container fleet-agent-03 is not running'; exit 1"}, Cols: 80, Rows: 24}
		return s.Run(ctx, c)
	})
	c = dial()
	c.until("an error frame", hasFrame(fleetapi.FrameError))
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.fr[len(c.fr)-1]; !strings.Contains(e.Error, "is not running") {
		t.Fatalf("error frame = %+v, want the reason", e)
	}
}

func TestTheSessionEndsWhenTheSocketCloses(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	dial, results := serve(t, func(ctx context.Context, c *Conn) Result {
		s := &Session{Argv: echo(pidfile), Cols: 80, Rows: 24, Input: true}
		return s.Run(ctx, c)
	})
	c := dial()
	c.until("the size", contains("24 80"))
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	c.c.Close(websocket.StatusNormalClosure, "")
	if r := result(t, results); !r.Killed {
		t.Fatalf("result = %+v, want the process killed", r)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("process %d still runs after the socket closed", pid)
	}
}

func TestAnIdleWatcherIsClosed(t *testing.T) {
	dial, results := serve(t, func(ctx context.Context, c *Conn) Result {
		s := &Session{Argv: echo(""), Cols: 80, Rows: 24, Idle: 50 * time.Millisecond}
		return s.Run(ctx, c)
	})
	c := dial()
	c.until("an exit frame", hasFrame(fleetapi.FrameExit))
	if r := result(t, results); !r.Killed {
		t.Fatalf("result = %+v, want killed for idling", r)
	}
}

func TestTheCommandPerMode(t *testing.T) {
	tg := Target{Worker: "agent-03:2", Container: target.Container{Exec: []string{"docker", "--context", "pc2"}, Name: "fleet-agent-03"}, Pane: "main-2"}
	for mode, want := range map[fleetapi.TermMode]string{
		fleetapi.TermWatch: "docker --context pc2 exec -it -e FLEET_TERM=id1 fleet-agent-03 tmux attach -t =main-2 -f read-only,ignore-size",
		fleetapi.TermType:  "docker --context pc2 exec -it -e FLEET_TERM=id1 fleet-agent-03 tmux attach -t =main-2",
		fleetapi.TermShell: "docker --context pc2 exec -it -e FLEET_TERM=id1 -w /work fleet-agent-03 bash -l",
	} {
		if got := strings.Join(Argv(tg, mode, "id1"), " "); got != want {
			t.Errorf("%s:\n got %s\nwant %s", mode, got, want)
		}
	}
}
