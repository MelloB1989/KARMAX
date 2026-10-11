// Package sshdial reaches a loopback-only service on another host through
// ssh: `ssh <host> -W <addr>` pipes stdio to addr from that host's side, and
// the pipes become a net.Conn. Nothing is opened on the LAN.
package sshdial

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Dialer returns a DialContext that ignores the address it is asked for and
// always connects to addr on host.
func Dialer(host, addr string) func(ctx context.Context, network, _ string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dial(ctx, host, addr)
	}
}

// Client is an http.Client whose connections go through ssh; it has no
// timeout of its own (WebSockets use it), so bound each request with a
// context. Pass it to websocket.DialOptions.HTTPClient too.
func Client(host, addr string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: Dialer(host, addr), DisableKeepAlives: false,
		IdleConnTimeout: 30 * time.Second}}
}

// Cat reads a file on host.
func Cat(ctx context.Context, host, path string) ([]byte, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", host, "cat", path)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ssh %s cat %s: %w: %s", host, path, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func dial(ctx context.Context, host, addr string) (net.Conn, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	var stderr bytes.Buffer
	// Not CommandContext: ctx only bounds the dial; the connection outlives it.
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", host, "-W", addr)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, &stderr
	if err := cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("ssh: %w", err)
	}
	inR.Close()
	outW.Close()
	c := &conn{cmd: cmd, r: outR, w: inW, stderr: &stderr, host: host, addr: addr, exited: make(chan struct{})}
	go func() { c.waitErr = cmd.Wait(); close(c.exited) }()
	return c, nil
}

type conn struct {
	cmd        *exec.Cmd
	r, w       *os.File
	stderr     *bytes.Buffer
	host, addr string
	exited     chan struct{}
	waitErr    error
	once       sync.Once
}

// explain adds ssh's own message to a failed read or write.
func (c *conn) explain(err error) error {
	if err == nil {
		return nil
	}
	select {
	case <-c.exited:
	case <-time.After(200 * time.Millisecond):
		return err
	}
	if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
		return fmt.Errorf("ssh %s -W %s: %s", c.host, c.addr, msg)
	}
	return err
}

func (c *conn) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		if n == 0 && errors.Is(err, os.ErrClosed) {
			return n, net.ErrClosed
		}
		if n == 0 {
			err = c.explain(err)
		}
	}
	return n, err
}

func (c *conn) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	return n, c.explain(err)
}

func (c *conn) Close() error {
	c.once.Do(func() {
		c.w.Close()
		if c.cmd.Process != nil {
			c.cmd.Process.Kill()
		}
		<-c.exited
		c.r.Close()
	})
	return nil
}

type addr string

func (a addr) Network() string { return "ssh" }
func (a addr) String() string  { return string(a) }

func (c *conn) LocalAddr() net.Addr  { return addr("local") }
func (c *conn) RemoteAddr() net.Addr { return addr(c.host + "->" + c.addr) }

func (c *conn) SetDeadline(t time.Time) error {
	c.r.SetReadDeadline(t)
	c.w.SetWriteDeadline(t)
	return nil
}
func (c *conn) SetReadDeadline(t time.Time) error  { return c.r.SetReadDeadline(t) }
func (c *conn) SetWriteDeadline(t time.Time) error { return c.w.SetWriteDeadline(t) }
