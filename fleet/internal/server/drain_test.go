package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
)

// pollOps holds a wait open until its context ends, like the real one, and
// answers from a closed ledger once closed is set.
type pollOps struct {
	fake
	started chan struct{}
	closed  chan struct{}
}

func (p *pollOps) Wait(ctx context.Context, _, _ string, _ int64) (daemon.WaitResult, error) {
	p.started <- struct{}{}
	<-ctx.Done()
	select {
	case <-p.closed:
		return daemon.WaitResult{}, errors.New("sql: database is closed")
	default:
	}
	return daemon.WaitResult{Reason: "timeout"}, nil
}

func TestDrainEndsAWaitInFlightWithA503BeforeTheLedgerCloses(t *testing.T) {
	ops := &pollOps{started: make(chan struct{}, 2), closed: make(chan struct{})}
	h, drain := DrainingHandler(ops, Tokens{Full: "full"}, nil)
	rec := make(chan *httptest.ResponseRecorder, 1)
	go func() { rec <- do(t, h, "GET", "/v1/wait?task=T1&timeout=1h", "full", "") }()
	<-ops.started
	drain()
	var got *httptest.ResponseRecorder
	select {
	case got = <-rec:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait was not ended by the drain")
	}
	close(ops.closed) // the ledger closes once the polls are out
	if got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), "shutting down") {
		t.Fatalf("drained wait = %d %s, want a clean 503", got.Code, got.Body)
	}
	// A poll that arrives after the drain is cut short at once.
	go func() { rec <- do(t, h, "GET", "/v1/wait?task=T1&timeout=1h", "full", "") }()
	select {
	case got = <-rec:
	case <-time.After(5 * time.Second):
		t.Fatal("a wait after the drain was held open")
	}
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("wait after the drain = %d, want 503", got.Code)
	}
}

func TestAWaitThatRunsOutOfTimeWithoutADrainIsStillATimeout(t *testing.T) {
	ops := &pollOps{started: make(chan struct{}, 1), closed: make(chan struct{})}
	h, _ := DrainingHandler(ops, Tokens{Full: "full"}, nil)
	got := do(t, h, "GET", "/v1/wait?task=T1&timeout=20ms", "full", "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"timeout"`) {
		t.Fatalf("wait = %d %s", got.Code, got.Body)
	}
}
