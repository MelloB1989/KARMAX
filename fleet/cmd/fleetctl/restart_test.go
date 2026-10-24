package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/server"
)

// waitOps is the part of fleetd that wait reaches: it holds a wait open until
// a message is delivered, and answers from a closed ledger once closed.
type waitOps struct {
	server.Ops
	msgs    chan ledger.Relay
	started chan struct{}
	closed  atomic.Bool
}

func (w *waitOps) Wait(ctx context.Context, _, _ string, _ int64) (daemon.WaitResult, error) {
	w.started <- struct{}{}
	select {
	case m := <-w.msgs:
		return daemon.WaitResult{Reason: "message", Message: &m}, nil
	case <-ctx.Done():
	}
	if w.closed.Load() {
		return daemon.WaitResult{}, errors.New("sql: database is closed")
	}
	return daemon.WaitResult{Reason: "timeout"}, nil
}

// fleetdAt starts the API on addr, as fleetd does, and returns how it stops:
// drain the polls, then shut the server down, then close the ledger.
func fleetdAt(t *testing.T, addr string, ops *waitOps) (stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	h, drain := server.DrainingHandler(ops, server.Tokens{Full: "tok"}, nil)
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	return func() {
		drain()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		ops.closed.Store(true)
	}
}

func TestWaitRetriesAcrossARealFleetdRestartAndReturnsTheMessageOnce(t *testing.T) {
	ops := &waitOps{msgs: make(chan ledger.Relay, 1), started: make(chan struct{}, 8)}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	stop := fleetdAt(t, addr, ops)

	type result struct {
		r   daemon.WaitResult
		err error
	}
	done := make(chan result, 1)
	var errOut bytes.Buffer
	sleep := func(ctx context.Context, d time.Duration) error { time.Sleep(50 * time.Millisecond); return ctx.Err() }
	go func() {
		r, err := waitFor(context.Background(), &client{base: "http://" + addr, token: "tok"}, "", "T1", 0, time.Minute, &errOut, sleep)
		done <- result{r, err}
	}()
	<-ops.started // the wait is in flight
	stop()        // fleetd restarts: the wait is ended with a 503, not from a closed ledger
	select {
	case res := <-done:
		t.Fatalf("wait ended across the restart: %+v, %v", res.r, res.err)
	case <-time.After(300 * time.Millisecond):
	}
	ops.closed.Store(false)
	stop = fleetdAt(t, addr, ops)
	defer stop()
	<-ops.started // the retried wait is with the new fleetd
	ops.msgs <- ledger.Relay{ID: 9, From: "agent-03", Text: "ready"}
	select {
	case res := <-done:
		if res.err != nil || res.r.Reason != "message" || res.r.Message.ID != 9 {
			t.Fatalf("wait = %+v, %v; want the message", res.r, res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wait did not return after fleetd was back")
	}
	if got := strings.Count(errOut.String(), "fleetd unreachable, retrying"); got != 1 {
		t.Errorf("stderr = %q, want the notice once", errOut.String())
	}
	if len(ops.msgs) != 0 {
		t.Error("the message was taken more than once")
	}
}
