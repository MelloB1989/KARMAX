package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// fakeTerm is a fleetd whose /v1/term runs serve on each socket.
func fakeTerm(t *testing.T, serve func(r *http.Request, c *websocket.Conn)) *client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, fleetapi.PathTerm) {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"a fleet token is required"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/agent-05") {
			w.WriteHeader(403)
			io.WriteString(w, `{"error":"agent-05 is not yours to use"}`)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		serve(r, c)
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, token: "tok"}
}

func sendFrame(c *websocket.Conn, f fleetapi.TermFrame) {
	b, _ := json.Marshal(f)
	c.Write(context.Background(), websocket.MessageText, b)
}

func readFrame(t *testing.T, c *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	typ, b, err := c.Read(ctx)
	if err != nil {
		t.Errorf("read: %v", err)
	}
	return typ, b
}

func size80x24() (int, int, bool) { return 80, 24, true }

// syncBuf is a terminal's output.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestSSHSendsResizeFirst(t *testing.T) {
	got := make(chan fleetapi.TermFrame, 2)
	var query string
	c := fakeTerm(t, func(r *http.Request, ws *websocket.Conn) {
		query = r.URL.RawQuery
		typ, b := readFrame(t, ws)
		var f fleetapi.TermFrame
		if typ != websocket.MessageText || json.Unmarshal(b, &f) != nil {
			t.Errorf("first message: %v %q, want a resize frame", typ, b)
		}
		got <- f
		// A terminal resized later sends another.
		typ, b = readFrame(t, ws)
		json.Unmarshal(b, &f)
		got <- f
		sendFrame(ws, fleetapi.TermFrame{Type: fleetapi.FrameExit})
	})
	winch := make(chan struct{}, 1)
	size := make(chan [2]int, 2)
	size <- [2]int{80, 24}
	size <- [2]int{132, 43}
	var last [2]int
	sizeFn := func() (int, int, bool) {
		select {
		case s := <-size:
			last = s
		default:
		}
		return last[0], last[1], true
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.term(context.Background(), termRun{worker: "agent-03:2", mode: fleetapi.TermType, in: strings.NewReader(""), out: io.Discard, size: sizeFn, winch: winch})
		done <- err
	}()
	if f := <-got; f.Type != fleetapi.FrameResize || f.Cols != 80 || f.Rows != 24 {
		t.Fatalf("first frame = %+v, want resize 80x24", f)
	}
	if !strings.Contains(query, "mode=type") || !strings.Contains(query, "cols=80") || !strings.Contains(query, "rows=24") {
		t.Fatalf("query = %q, want the mode and the size", query)
	}
	winch <- struct{}{}
	if f := <-got; f.Type != fleetapi.FrameResize || f.Cols != 132 || f.Rows != 43 {
		t.Fatalf("after SIGWINCH = %+v, want resize 132x43", f)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCtrlBracketDetaches(t *testing.T) {
	typed := make(chan string, 1)
	c := fakeTerm(t, func(r *http.Request, ws *websocket.Conn) {
		readFrame(t, ws) // the resize
		sendFrame(ws, fleetapi.TermFrame{Type: fleetapi.FrameHello, Worker: "agent-03", Mode: fleetapi.TermType})
		ws.Write(context.Background(), websocket.MessageBinary, []byte("the screen"))
		var b strings.Builder
		for {
			typ, p, err := ws.Read(context.Background())
			if err != nil {
				break
			}
			if typ == websocket.MessageBinary {
				b.Write(p)
			}
		}
		typed <- b.String()
	})
	pr, pw := io.Pipe()
	out := &syncBuf{}
	done := make(chan struct{})
	var how string
	var err error
	go func() {
		defer close(done)
		how, err = c.term(context.Background(), termRun{worker: "agent-03", mode: fleetapi.TermType, in: pr, out: out, size: size80x24})
	}()
	pw.Write([]byte("ls\r"))
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), "the screen"); {
		if time.Now().After(deadline) {
			t.Fatalf("no screen; output %q", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	pw.Write([]byte("ab\x1dnever sent"))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Ctrl-] did not detach")
	}
	if err != nil || how != "[detached from agent-03]" {
		t.Fatalf("how %q, err %v", how, err)
	}
	if s := <-typed; s != "ls\rab" {
		t.Fatalf("fleetd got %q, want what came before Ctrl-] only", s)
	}
}

func TestAHeldFrameExitsWithTheHolder(t *testing.T) {
	c := fakeTerm(t, func(r *http.Request, ws *websocket.Conn) {
		readFrame(t, ws)
		sendFrame(ws, fleetapi.TermFrame{Type: fleetapi.FrameHeld, Worker: "agent-03", Holder: "friend@example.com (friend-laptop.tail1234.ts.net)"})
		ws.Close(websocket.StatusNormalClosure, "")
	})
	_, err := c.term(context.Background(), termRun{worker: "agent-03", mode: fleetapi.TermType, in: strings.NewReader(""), out: io.Discard, size: size80x24})
	if err == nil || !strings.HasPrefix(err.Error(), "held by friend@example.com (friend-laptop.tail1234.ts.net)") {
		t.Fatalf("err = %v, want held by the holder", err)
	}
}

func TestAnErrorFrameIsTheError(t *testing.T) {
	c := fakeTerm(t, func(r *http.Request, ws *websocket.Conn) {
		readFrame(t, ws)
		sendFrame(ws, fleetapi.TermFrame{Type: fleetapi.FrameError, Error: "Error response from daemon: container fleet-agent-03 is not running"})
		ws.Close(websocket.StatusNormalClosure, "")
	})
	how, err := c.term(context.Background(), termRun{worker: "agent-03", mode: fleetapi.TermWatch, in: strings.NewReader(""), out: io.Discard, size: size80x24})
	if err == nil || !strings.Contains(err.Error(), "is not running") || how != "" {
		t.Fatalf("how %q, err %v", how, err)
	}
}

func TestAReleasedTypistWatchesOn(t *testing.T) {
	c := fakeTerm(t, func(r *http.Request, ws *websocket.Conn) {
		readFrame(t, ws)
		sendFrame(ws, fleetapi.TermFrame{Type: fleetapi.FrameReleased, Holder: "operator"})
		sendFrame(ws, fleetapi.TermFrame{Type: fleetapi.FrameHello, Mode: fleetapi.TermWatch})
		ws.Write(context.Background(), websocket.MessageBinary, []byte("still here"))
		sendFrame(ws, fleetapi.TermFrame{Type: fleetapi.FrameExit})
	})
	out := &syncBuf{}
	how, err := c.term(context.Background(), termRun{worker: "agent-03", mode: fleetapi.TermType, in: strings.NewReader(""), out: out, size: size80x24})
	if err != nil || how != "[detached from agent-03]" {
		t.Fatalf("how %q, err %v", how, err)
	}
	if s := out.String(); !strings.Contains(s, "operator took the keyboard of agent-03: you watch now") || !strings.Contains(s, "still here") {
		t.Fatalf("output %q", s)
	}
}

func TestARefusedUpgradeSaysWhy(t *testing.T) {
	c := fakeTerm(t, func(*http.Request, *websocket.Conn) {})
	_, err := c.term(context.Background(), termRun{worker: "agent-05", mode: fleetapi.TermWatch, in: strings.NewReader(""), out: io.Discard, size: size80x24})
	if err == nil || err.Error() != "agent-05 is not yours to use" {
		t.Fatalf("err = %v", err)
	}
}

func TestSSHFlags(t *testing.T) {
	if err := run("ssh", []string{"agent-03", "--type", "--shell"}); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("--type --shell: %v", err)
	}
	if err := run("ssh", nil); err == nil || !strings.Contains(err.Error(), "ssh <agent>") {
		t.Fatalf("no worker: %v", err)
	}
}

// session runs a term session against a fake fleetd that shows a screen,
// types what, and returns how it left and what fleetd was sent.
func session(t *testing.T, mode fleetapi.TermMode, what string) (how, typed string) {
	t.Helper()
	got := make(chan string, 1)
	c := fakeTerm(t, func(r *http.Request, ws *websocket.Conn) {
		readFrame(t, ws)
		sendFrame(ws, fleetapi.TermFrame{Type: fleetapi.FrameHello, Worker: "agent-03", Mode: mode})
		ws.Write(context.Background(), websocket.MessageBinary, []byte("the screen"))
		var b strings.Builder
		for {
			typ, p, err := ws.Read(context.Background())
			if err != nil {
				break
			}
			if typ == websocket.MessageBinary {
				b.Write(p)
			}
		}
		got <- b.String()
	})
	pr, pw := io.Pipe()
	out := &syncBuf{}
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		how, err = c.term(context.Background(), termRun{worker: "agent-03", mode: mode, in: pr, out: out, size: size80x24})
	}()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), "the screen"); {
		if time.Now().After(deadline) {
			t.Fatalf("no screen; output %q", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	pw.Write([]byte(what))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%q did not leave a %s session", what, mode)
	}
	if err != nil {
		t.Fatal(err)
	}
	return how, <-got
}

// On a terminal an application in the session asked for extended keys,
// Ctrl-] comes as an escape sequence; it leaves all the same, and a
// watcher also leaves on q.
func TestASessionIsLeftWhateverTheTerminalSends(t *testing.T) {
	for _, c := range []struct {
		mode       fleetapi.TermMode
		what, sent string
	}{
		{fleetapi.TermType, "ls\r\x1b[93;5u", "ls\r"},
		{fleetapi.TermType, "x\x1b[27;5;93~", "x"},
		{fleetapi.TermShell, "\x1b[93;5:1u", ""},
		{fleetapi.TermWatch, "q", ""},
	} {
		how, sent := session(t, c.mode, c.what)
		if how != "[detached from agent-03]" || sent != c.sent {
			t.Errorf("%s %q: how %q, fleetd got %q, want %q", c.mode, c.what, how, sent, c.sent)
		}
	}
}
