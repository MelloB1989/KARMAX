package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/coder/websocket"
	"github.com/muesli/cancelreader"
	"golang.org/x/term"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// detachKey is Ctrl-]: it ends the session here, whatever the mode.
const detachKey = 0x1d

// sshCmd is `ssh <worker> [--type] [--shell] [--force]`, and attach, which
// is ssh --type: a worker's live session in this terminal, through fleetd.
func sshCmd(cmd string, args []string) error {
	var typ, shell, force bool
	_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&typ, "type", cmd == "attach", "")
		fs.BoolVar(&shell, "shell", false, "")
		fs.BoolVar(&force, "force", false, "")
	})
	if err != nil {
		return err
	}
	if err := need(pos, 1, cmd+" <agent> [--type] [--shell] [--force]"); err != nil {
		return err
	}
	mode := fleetapi.TermWatch
	switch {
	case typ && shell:
		return errors.New("--type or --shell, not both")
	case shell:
		mode = fleetapi.TermShell
	case typ:
		mode = fleetapi.TermType
	}
	return attach(pos[0], mode, force, os.Stdin, os.Stdout, os.Stderr)
}

// AttachCommand is a terminal session on a worker for the TUI to suspend
// into (tea.Exec): the same as fleetctl ssh.
func AttachCommand(worker string, mode fleetapi.TermMode) tea.ExecCommand {
	return &attachCmd{worker: worker, mode: mode, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
}

type attachCmd struct {
	worker         string
	mode           fleetapi.TermMode
	stdin          io.Reader
	stdout, stderr io.Writer
}

func (a *attachCmd) Run() error            { return attach(a.worker, a.mode, false, a.stdin, a.stdout, a.stderr) }
func (a *attachCmd) SetStdin(r io.Reader)  { a.stdin = r }
func (a *attachCmd) SetStdout(w io.Writer) { a.stdout = w }
func (a *attachCmd) SetStderr(w io.Writer) { a.stderr = w }

// attach runs the session with this terminal in raw mode, and says how it
// left.
func attach(worker string, mode fleetapi.TermMode, force bool, stdin io.Reader, stdout, stderr io.Writer) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	in, out := fdOf(stdin), fdOf(stdout)
	size := func() (int, int, bool) {
		if out < 0 {
			return 0, 0, false
		}
		w, h, err := term.GetSize(out)
		return w, h, err == nil && w > 0 && h > 0
	}
	if in >= 0 && term.IsTerminal(in) {
		old, err := term.MakeRaw(in)
		if err != nil {
			return err
		}
		defer term.Restore(in, old)
		defer enableVT(out)()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	how, err := c.term(ctx, termRun{worker: worker, mode: mode, force: force, in: stdin, out: stdout, size: size, winch: winch(ctx, size)})
	if how != "" {
		fmt.Fprintf(stderr, "\r\n%s\r\n", how)
	}
	return err
}

func fdOf(v any) int {
	if f, ok := v.(*os.File); ok {
		return int(f.Fd())
	}
	return -1
}

// termRun is one ssh session's ends.
type termRun struct {
	worker string
	mode   fleetapi.TermMode
	force  bool
	in     io.Reader
	out    io.Writer
	size   func() (cols, rows int, ok bool)
	winch  <-chan struct{} // the terminal changed size
}

// term runs a session on fleetd's /v1/term until Ctrl-], or fleetd ends it.
// how is the line that says how it left; err is why it could not go on.
func (c *client) term(ctx context.Context, t termRun) (how string, err error) {
	q := url.Values{"mode": {string(t.mode)}}
	cols, rows, sized := t.size()
	if sized {
		q.Set("cols", strconv.Itoa(cols))
		q.Set("rows", strconv.Itoa(rows))
	}
	if t.force {
		q.Set("force", "1")
	}
	u := "ws" + strings.TrimPrefix(c.base, "http") + fleetapi.PathTerm + url.PathEscape(t.worker) + "?" + q.Encode()
	h := http.Header{}
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return "", termDialError(c.base, resp, err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(16 << 20)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	frame := func(f fleetapi.TermFrame) error {
		b, _ := json.Marshal(f)
		return ws.Write(ctx, websocket.MessageText, b)
	}
	if sized { // first, so the session starts at this terminal's size
		if err := frame(fleetapi.TermFrame{Type: fleetapi.FrameResize, Cols: cols, Rows: rows}); err != nil {
			return "", fmt.Errorf("fleetd: %w", err)
		}
	}

	detached := make(chan struct{})
	go func() { // keystrokes, up to Ctrl-]
		in, err := cancelreader.NewReader(t.in)
		if err != nil {
			return
		}
		defer in.Close()
		go func() { <-ctx.Done(); in.Cancel() }() // never left reading what comes after
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			b := buf[:n]
			if i := bytes.IndexByte(b, detachKey); i >= 0 {
				if i > 0 {
					ws.Write(ctx, websocket.MessageBinary, b[:i])
				}
				close(detached)
				return
			}
			if n > 0 && ws.Write(ctx, websocket.MessageBinary, b) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { // resizes
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.winch:
				if c, r, ok := t.size(); ok {
					frame(fleetapi.TermFrame{Type: fleetapi.FrameResize, Cols: c, Rows: r})
				}
			}
		}
	}()

	type msg struct {
		typ websocket.MessageType
		b   []byte
		err error
	}
	msgs := make(chan msg)
	go func() {
		for {
			typ, b, err := ws.Read(ctx)
			select {
			case msgs <- msg{typ, b, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	left := "[detached from " + t.worker + "]"
	for {
		select {
		case <-detached:
			ws.Close(websocket.StatusNormalClosure, "")
			return left, nil
		case m := <-msgs:
			if m.err != nil {
				if websocket.CloseStatus(m.err) == websocket.StatusNormalClosure {
					return left, nil
				}
				return "", fmt.Errorf("fleetd: the session broke: %w", m.err)
			}
			if m.typ == websocket.MessageBinary {
				t.out.Write(m.b)
				continue
			}
			var f fleetapi.TermFrame
			if json.Unmarshal(m.b, &f) != nil {
				continue
			}
			switch f.Type {
			case fleetapi.FrameHeld:
				return "", fmt.Errorf("held by %s: %s keeps the keyboard; watch with fleetctl ssh %s, or (an admin) take it with --force", f.Holder, f.Holder, t.worker)
			case fleetapi.FrameError:
				return "", errors.New(f.Error)
			case fleetapi.FrameReleased:
				fmt.Fprintf(t.out, "\r\n[%s took the keyboard of %s: you watch now; Ctrl-] leaves]\r\n", orSomeone(f.Holder), t.worker)
			case fleetapi.FrameExit:
				if f.Error != "" {
					return left + " " + f.Error, nil
				}
				return left, nil
			}
		}
	}
}

func orSomeone(s string) string {
	if s == "" {
		return "an admin"
	}
	return s
}

// termDialError is what fleetd said when it refused the upgrade.
func termDialError(base string, resp *http.Response, err error) error {
	if resp == nil {
		return fmt.Errorf("fleetd unreachable at %s: %w", base, err)
	}
	var b []byte
	if resp.Body != nil {
		b, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
	}
	var e struct{ Error string }
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return &statusError{Code: resp.StatusCode, msg: e.Error, Body: b}
	}
	return &statusError{Code: resp.StatusCode, msg: "fleetd: " + resp.Status, Body: b}
}
