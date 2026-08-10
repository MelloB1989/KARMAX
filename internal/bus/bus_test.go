package bus

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "k.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestLog(t *testing.T, s *store.Store) *Log {
	t.Helper()
	l := NewLog(s, store.DefaultWorkspace, zap.NewNop())
	l.retryDelay = func(int) time.Duration { return time.Millisecond }
	return l
}

func mustPublish(t *testing.T, l *Log, kind EventKind, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := l.Publish(NewEvent(kind, "agent", map[string]any{"i": i})); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
}

// The bug this replaced: a subscriber that was busy when an event arrived got a
// warning and nothing else.
func TestASlowSubscriberLosesNothing(t *testing.T) {
	l := newTestLog(t, testStore(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const total = 300
	var (
		mu   sync.Mutex
		seen []int
	)
	done := make(chan struct{})
	l.Consume(ctx, "slow", []EventKind{EventUserDefined}, func(_ context.Context, e Event) error {
		// Slower than the publisher, which is the whole point.
		time.Sleep(time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, int(e.Payload["i"].(float64)))
		if len(seen) == total {
			close(done)
		}
		return nil
	})

	mustPublish(t, l, EventUserDefined, total)

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		mu.Lock()
		got := len(seen)
		mu.Unlock()
		t.Fatalf("only %d of %d events were delivered", got, total)
	}

	// And in order, because a log delivered out of order is not a log.
	mu.Lock()
	defer mu.Unlock()
	for i, v := range seen {
		if v != i {
			t.Fatalf("event %d arrived at position %d", v, i)
		}
	}
}

func TestASubscriberResumesWhereItStopped(t *testing.T) {
	s := testStore(t)
	l := newTestLog(t, s)

	// Started BEFORE anything is published: a new subscriber begins at the head,
	// so events that predate it are not its business.
	first := make(chan Event, 8)
	ctx1, cancel1 := context.WithCancel(context.Background())
	l.Consume(ctx1, "resumer", []EventKind{EventUserDefined}, func(_ context.Context, e Event) error {
		first <- e
		return nil
	})
	mustPublish(t, l, EventUserDefined, 5)
	for i := 0; i < 5; i++ {
		select {
		case <-first:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d events arrived before the restart", i)
		}
	}
	cancel1()

	// A fresh Log over the same database is what a restart looks like.
	mustPublish(t, l, EventUserDefined, 3)
	l2 := newTestLog(t, s)
	second := make(chan Event, 8)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	l2.Consume(ctx2, "resumer", []EventKind{EventUserDefined}, func(_ context.Context, e Event) error {
		second <- e
		return nil
	})

	for i := 0; i < 3; i++ {
		select {
		case e := <-second:
			// The five already handled must not come back.
			if got := int(e.Payload["i"].(float64)); got != i {
				t.Fatalf("after restart got event %d, want %d — old events were replayed", got, i)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("events published while the subscriber was down never arrived")
		}
	}
	select {
	case e := <-second:
		t.Fatalf("an already-handled event was redelivered: %+v", e.Payload)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestAFailingHandlerRetriesAndThenDeadLetters(t *testing.T) {
	s := testStore(t)
	l := newTestLog(t, s)

	var attempts int
	var mu sync.Mutex
	dead := make(chan store.DeadLetter, 1)
	l.OnDeadLetter(func(d store.DeadLetter) { dead <- d })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	moved := make(chan struct{}, 1)
	l.Consume(ctx, "flaky", []EventKind{EventUserDefined}, func(_ context.Context, e Event) error {
		mu.Lock()
		defer mu.Unlock()
		if e.Payload["i"].(float64) == 0 {
			attempts++
			return fmt.Errorf("nope")
		}
		moved <- struct{}{}
		return nil
	})

	mustPublish(t, l, EventUserDefined, 2)

	select {
	case d := <-dead:
		if d.Subscriber != "flaky" || d.Attempts != maxAttempts {
			t.Errorf("dead letter = %+v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a permanently failing event was never dead-lettered")
	}

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != maxAttempts {
		t.Errorf("handler was called %d times, want %d", got, maxAttempts)
	}

	// The poisonous event must not block everything behind it.
	select {
	case <-moved:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscriber stalled on the dead-lettered event")
	}

	letters, err := s.DeadLetters(10)
	if err != nil || len(letters) != 1 {
		t.Fatalf("dead letters = %v, err = %v", letters, err)
	}
}

func TestAPanickingHandlerDoesNotKillTheSubscriber(t *testing.T) {
	l := newTestLog(t, testStore(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	survived := make(chan struct{}, 1)
	l.Consume(ctx, "panicky", []EventKind{EventUserDefined}, func(_ context.Context, e Event) error {
		if e.Payload["i"].(float64) == 0 {
			panic("loop code is third-party and this happens")
		}
		survived <- struct{}{}
		return nil
	})

	mustPublish(t, l, EventUserDefined, 2)
	select {
	case <-survived:
	case <-time.After(10 * time.Second):
		t.Fatal("a panic in one handler stopped the subscriber")
	}
}

func TestSubscribersAdvanceIndependently(t *testing.T) {
	s := testStore(t)
	l := newTestLog(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fast := make(chan Event, 16)
	block := make(chan struct{})
	l.Consume(ctx, "fast", []EventKind{EventUserDefined}, func(_ context.Context, e Event) error {
		fast <- e
		return nil
	})
	l.Consume(ctx, "stuck", []EventKind{EventUserDefined}, func(ctx context.Context, e Event) error {
		<-block
		return nil
	})

	mustPublish(t, l, EventUserDefined, 3)
	for i := 0; i < 3; i++ {
		select {
		case <-fast:
		case <-time.After(5 * time.Second):
			t.Fatal("one blocked subscriber held up another")
		}
	}
	close(block)

	// Kinds are filtered per subscriber, not globally.
	if err := l.Publish(NewEvent(EventToolResult, "agent", nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-fast:
		t.Fatalf("a subscriber received a kind it never asked for: %s", e.Kind)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestPruningKeepsWhatNobodyHasReadYet(t *testing.T) {
	s := testStore(t)
	l := newTestLog(t, s)
	mustPublish(t, l, EventUserDefined, 4)

	// A subscriber that has read nothing holds the whole log.
	if err := s.SetConsumerOffset("lagging", store.DefaultWorkspace, 0); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneEventLog(store.DefaultWorkspace, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("pruned %d events a subscriber had not read", n)
	}

	// Once it has caught up, the same prune is allowed.
	if err := s.SetConsumerOffset("lagging", store.DefaultWorkspace, 4); err != nil {
		t.Fatal(err)
	}
	n, err = s.PruneEventLog(store.DefaultWorkspace, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("pruned %d events, want 4", n)
	}
}

func TestOffsetsNeverGoBackwards(t *testing.T) {
	s := testStore(t)
	if err := s.SetConsumerOffset("x", store.DefaultWorkspace, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConsumerOffset("x", store.DefaultWorkspace, 3); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.ConsumerOffset("x", store.DefaultWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if got != 10 {
		t.Errorf("offset moved backwards to %d", got)
	}
}

func TestPublishingTheSameEventTwiceAppendsOnce(t *testing.T) {
	s := testStore(t)
	l := newTestLog(t, s)
	e := NewEvent(EventUserDefined, "agent", map[string]any{"i": 0})

	if err := l.Publish(e); err != nil {
		t.Fatal(err)
	}
	if err := l.Publish(e); err != nil {
		t.Fatalf("republishing the same event should be idempotent: %v", err)
	}
	got, err := s.LogEventsAfter(store.DefaultWorkspace, 0, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("the same event id was appended %d times", len(got))
	}
}

// The bug this pins, found by restarting a real daemon: a subscriber that had
// never run started at offset 0 and replayed the entire retained history. For
// the agent router that meant re-delivering every WhatsApp message ever
// received, and the agent would have answered conversations long finished.
//
// Durability exists so nothing is lost while a subscriber is DOWN. A subscriber
// that never existed missed nothing.
func TestANewSubscriberStartsAtTheHeadNotAtTheBeginning(t *testing.T) {
	s := testStore(t)
	l := newTestLog(t, s)

	// A history it was not around for.
	mustPublish(t, l, EventUserDefined, 50)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Event, 64)
	l.Consume(ctx, "newcomer", []EventKind{EventUserDefined}, func(_ context.Context, e Event) error {
		got <- e
		return nil
	})

	// Nothing historical.
	select {
	case e := <-got:
		t.Fatalf("a new subscriber replayed history: got event %v", e.Payload["i"])
	case <-time.After(500 * time.Millisecond):
	}

	// But everything from now on.
	if err := l.Publish(NewEvent(EventUserDefined, "agent", map[string]any{"i": 999})); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-got:
		if e.Payload["i"].(float64) != 999 {
			t.Errorf("received %v, want the new event", e.Payload["i"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a new subscriber missed a live event")
	}
}

// The other half: a subscriber that HAS run picks up where it left off, so a
// restart loses nothing. Fixing the replay bug must not break this.
func TestAKnownSubscriberStillResumesFromItsOffset(t *testing.T) {
	s := testStore(t)
	l := newTestLog(t, s)

	// It ran once and got to seq 0 — recorded, so it is "known".
	if err := s.SetConsumerOffset("returning", store.DefaultWorkspace, 0); err != nil {
		t.Fatal(err)
	}
	mustPublish(t, l, EventUserDefined, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Event, 8)
	l.Consume(ctx, "returning", []EventKind{EventUserDefined}, func(_ context.Context, e Event) error {
		got <- e
		return nil
	})

	// All three, because it was down while they happened.
	for i := 0; i < 3; i++ {
		select {
		case <-got:
		case <-time.After(5 * time.Second):
			t.Fatalf("a returning subscriber lost event %d — durability is the point", i)
		}
	}
}
