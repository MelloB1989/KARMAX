package console

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func countingFill(n *atomic.Int64) func(*Snapshot) {
	return func(s *Snapshot) {
		*s = fixtureSnapshot()
		s.Today.TasksClosed = int(n.Add(1))
	}
}

func recv(t *testing.T, ch <-chan Message) Message {
	t.Helper()
	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no message")
	}
	return Message{}
}

func TestSubscriberGetsSnapshotThenPatches(t *testing.T) {
	var n atomic.Int64
	h := newHub(countingFill(&n), 10*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)
	ch, unsub := h.Subscribe(ctx)
	defer unsub()
	first := recv(t, ch)
	if first.Type != "snapshot" || first.Snapshot == nil || len(first.Snapshot.Agents) != 4 {
		t.Fatalf("first = %+v", first)
	}
	h.Changed()
	p := recv(t, ch)
	if p.Type != "patch" || p.Seq != first.Seq+1 || len(p.Ops) != 1 || p.Ops[0].Path != "/today/tasks_closed" {
		t.Fatalf("patch = %+v", p)
	}
	h.Changed()
	if p2 := recv(t, ch); p2.Seq != p.Seq+1 {
		t.Fatalf("seq %d after %d", p2.Seq, p.Seq)
	}
}

func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	var n atomic.Int64
	h := newHub(countingFill(&n), 0, nil)
	h.Snapshot()
	ch, _ := h.Subscribe(context.Background()) // never read
	done := make(chan struct{})
	go func() {
		for range subBuffer + 10 {
			h.rebuild()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a slow subscriber blocked the hub")
	}
	got := 0
	for range ch { // drains the buffer, then sees the close
		got++
	}
	if got != subBuffer {
		t.Fatalf("buffered %d messages", got)
	}
	// a fresh subscriber is unaffected
	ch2, c2 := h.Subscribe(context.Background())
	defer c2()
	if m := recv(t, ch2); m.Type != "snapshot" {
		t.Fatalf("%+v", m)
	}
}

func TestRebuildsAreBatchedToTenPerSecond(t *testing.T) {
	var n atomic.Int64
	h := newHub(countingFill(&n), hubInterval, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)
	end := time.Now().Add(time.Second)
	for time.Now().Before(end) {
		h.Changed()
		time.Sleep(time.Millisecond)
	}
	if got := n.Load(); got > 12 || got < 5 {
		t.Fatalf("%d rebuilds in a second of constant change, want about 10", got)
	}
}
