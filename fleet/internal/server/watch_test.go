package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// The plain fake's fleet never changes: a watch on it waits out its time.
func (f *fake) Snapshot(_ time.Time, all bool, agents []string, me fleetapi.Me) *fleetapi.Snapshot {
	return &fleetapi.Snapshot{Seq: 1, Me: me}
}
func (f *fake) SnapshotSeq() uint64      { return 1 }
func (f *fake) Changed() <-chan struct{} { return nil }
func (f *fake) KnockCount() int          { return 0 }

// streamOps is a fleet whose snapshot moves when the test says.
type streamOps struct {
	fake
	mu      sync.Mutex
	seq     uint64
	changed chan struct{}
	waiting chan struct{} // a watch took Changed: it is about to wait
	asked   []string      // each Snapshot's scope: all or the agents, and me
	knocks  int
}

func newStreamOps(seq uint64) *streamOps {
	return &streamOps{seq: seq, changed: make(chan struct{}), waiting: make(chan struct{}, 8)}
}

func (s *streamOps) Snapshot(_ time.Time, all bool, agents []string, me fleetapi.Me) *fleetapi.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := "all"
	if !all {
		scope = "agents[" + strings.Join(agents, ",") + "]"
	}
	s.asked = append(s.asked, scope+" "+me.Name+" "+string(me.Role))
	return &fleetapi.Snapshot{Seq: s.seq, Me: me, Workers: []fleetapi.Worker{}}
}
func (s *streamOps) SnapshotSeq() uint64 { s.mu.Lock(); defer s.mu.Unlock(); return s.seq }
func (s *streamOps) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.waiting <- struct{}{}:
	default:
	}
	return s.changed
}
func (s *streamOps) KnockCount() int { return s.knocks }

func (s *streamOps) bump() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	close(s.changed)
	s.changed = make(chan struct{})
}

func snapshotOf(t *testing.T, rec *httptest.ResponseRecorder) fleetapi.Snapshot {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("watch = %d %s", rec.Code, rec.Body)
	}
	var s fleetapi.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWatchAnswersAtOnceWhenBehind(t *testing.T) {
	ops := newStreamOps(5)
	h := Handler(ops, Tokens{Full: "full"}, nil)
	for _, since := range []string{"", "0", "3", "9"} { // 9: a sequence from before fleetd restarted
		if s := snapshotOf(t, do(t, h, "GET", "/v1/watch?since="+since, "full", "")); s.Seq != 5 {
			t.Errorf("since %q: seq %d", since, s.Seq)
		}
	}
	if rec := do(t, h, "GET", "/v1/watch?since=-1", "full", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("since -1 = %d", rec.Code)
	}
}

func TestWatchWaitsForAChange(t *testing.T) {
	ops := newStreamOps(5)
	h := Handler(ops, Tokens{Full: "full"}, nil)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- do(t, h, "GET", "/v1/watch?since=5", "full", "") }()
	<-ops.waiting
	select {
	case rec := <-done:
		t.Fatalf("the watch answered before a change: %d %s", rec.Code, rec.Body)
	default:
	}
	ops.bump()
	select {
	case rec := <-done:
		if s := snapshotOf(t, rec); s.Seq != 6 {
			t.Fatalf("seq = %d, want 6", s.Seq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not answer the change")
	}
}

func TestWatchAnswersWithTheSameSnapshotAfterItsTime(t *testing.T) {
	old := watchWait
	watchWait = 20 * time.Millisecond
	t.Cleanup(func() { watchWait = old })
	ops := newStreamOps(5)
	h := Handler(ops, Tokens{Full: "full"}, nil)
	if s := snapshotOf(t, do(t, h, "GET", "/v1/watch?since=5", "full", "")); s.Seq != 5 {
		t.Fatalf("seq = %d", s.Seq)
	}
}

func TestWatchEndsOnDrain(t *testing.T) {
	ops := newStreamOps(5)
	h, drain := DrainingHandler(ops, Tokens{Full: "full"}, nil)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- do(t, h, "GET", "/v1/watch?since=5", "full", "") }()
	<-ops.waiting
	drain()
	select {
	case rec := <-done:
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "shutting down") {
			t.Fatalf("drained watch = %d %s", rec.Code, rec.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the drain did not end the watch")
	}
}

func TestWatchIsScopedToTheCaller(t *testing.T) {
	ops := newStreamOps(5)
	ops.knocks = 3
	h := Handler(ops, Tokens{Full: "full", Relay: "relay", Console: "console"}, nil)

	if s := snapshotOf(t, do(t, h, "GET", "/v1/watch", "full", "")); s.Knocks != 3 || s.Me.Role != fleetapi.RoleAdmin {
		t.Errorf("full: knocks %d, me %+v", s.Knocks, s.Me)
	}
	if s := snapshotOf(t, do(t, h, "GET", "/v1/watch", "console", "")); s.Knocks != 0 {
		t.Errorf("console: knocks %d", s.Knocks)
	}
	if rec := do(t, h, "GET", "/v1/watch", "relay", ""); rec.Code != http.StatusForbidden {
		t.Errorf("relay: %d", rec.Code)
	}
	// A person on the tailnet who is not an admin: their lends, never all.
	req := httptest.NewRequest("GET", "/v1/watch", nil)
	req = req.WithContext(context.WithValue(req.Context(), callerKey{}, &Caller{Login: "friend@example.com", Role: "member"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if s := snapshotOf(t, rec); s.Knocks != 0 || s.Me.Name != "friend@example.com" || s.Me.Role != fleetapi.RoleGuest {
		t.Errorf("guest: knocks %d, me %+v", s.Knocks, s.Me)
	}
	want := []string{"all operator admin", "all operator admin", "agents[] friend@example.com guest"}
	if strings.Join(ops.asked, "; ") != strings.Join(want, "; ") {
		t.Errorf("snapshots asked = %q, want %q", ops.asked, want)
	}
}
