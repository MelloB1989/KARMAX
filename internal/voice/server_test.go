package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"
)

// A brain that answers slowly enough to be talked over, and that has one
// unprompted thing to say.
type testBrain struct {
	delay   time.Duration
	notices chan Reply
	ended   chan struct{}
	seen    []Utterance
}

func (b *testBrain) Greeting(context.Context, string) string { return "hi" }
func (b *testBrain) Answer(ctx context.Context, u Utterance) (Reply, error) {
	b.seen = append(b.seen, u)
	select {
	case <-time.After(b.delay):
	case <-ctx.Done():
	}
	if strings.Contains(u.Text, "bye") {
		return Reply{Text: "bye then", Hangup: true}, nil
	}
	return Reply{Text: "answer to " + u.Text}, nil
}
func (b *testBrain) Notices() <-chan Reply { return b.notices }
func (b *testBrain) End()                  { close(b.ended) }

func dial(t *testing.T, brain Brain) (*websocket.Conn, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ServeConversation(r.Context(), c, func() Brain { return brain }, zap.NewNop())
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn, func() { cancel(); conn.CloseNow(); srv.Close() }
}

func send(t *testing.T, c *websocket.Conn, m wire) {
	t.Helper()
	data, _ := json.Marshal(m)
	if err := c.Write(context.Background(), websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}

func recv(t *testing.T, c *websocket.Conn, within time.Duration) (wire, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		return wire{}, false
	}
	var m wire
	_ = json.Unmarshal(data, &m)
	return m, true
}

func TestStaleReplyIsNotSpoken(t *testing.T) {
	brain := &testBrain{delay: 400 * time.Millisecond, notices: make(chan Reply, 1), ended: make(chan struct{})}
	conn, done := dial(t, brain)
	defer done()
	send(t, conn, wire{Type: "start", CallID: "t"})
	if g, ok := recv(t, conn, 2*time.Second); !ok || g.Text != "hi" {
		t.Fatalf("greeting = %+v", g)
	}
	// Two utterances in quick succession: the caller talked past the first.
	send(t, conn, wire{Type: "utterance", ID: 1, Text: "first"})
	time.Sleep(50 * time.Millisecond)
	send(t, conn, wire{Type: "utterance", ID: 2, Text: "second"})

	m, ok := recv(t, conn, 3*time.Second)
	if !ok {
		t.Fatal("no reply")
	}
	if m.For != 2 || m.Text != "answer to second" {
		t.Fatalf("the first spoken reply must answer the newest utterance, got %+v", m)
	}
	// And nothing for the first ever arrives.
	if extra, ok := recv(t, conn, 300*time.Millisecond); ok {
		t.Fatalf("stale reply leaked: %+v", extra)
	}
}

func TestNoticeIsSpokenUnpromptedAndInterruptedIsPassed(t *testing.T) {
	brain := &testBrain{delay: 10 * time.Millisecond, notices: make(chan Reply, 1), ended: make(chan struct{})}
	conn, done := dial(t, brain)
	defer done()
	send(t, conn, wire{Type: "start", CallID: "t"})
	recv(t, conn, 2*time.Second) // greeting

	brain.notices <- Reply{Text: "your task finished"}
	m, ok := recv(t, conn, 2*time.Second)
	if !ok || m.Type != "say" || m.For != 0 || m.Text != "your task finished" {
		t.Fatalf("notice = %+v", m)
	}

	send(t, conn, wire{Type: "utterance", ID: 1, Text: "go on", Interrupted: true})
	if r, ok := recv(t, conn, 2*time.Second); !ok || r.For != 1 {
		t.Fatalf("reply = %+v", r)
	}
	if len(brain.seen) != 1 || !brain.seen[0].Interrupted {
		t.Fatalf("interrupted flag did not reach the brain: %+v", brain.seen)
	}
}

func TestHangupFollowsGoodbyeAndEndsTheBrain(t *testing.T) {
	brain := &testBrain{delay: 10 * time.Millisecond, notices: make(chan Reply, 1), ended: make(chan struct{})}
	conn, done := dial(t, brain)
	defer done()
	send(t, conn, wire{Type: "start", CallID: "t"})
	recv(t, conn, 2*time.Second)
	send(t, conn, wire{Type: "utterance", ID: 1, Text: "ok bye"})
	first, _ := recv(t, conn, 2*time.Second)
	second, _ := recv(t, conn, 2*time.Second)
	if first.Type != "say" || first.Text != "bye then" || second.Type != "hangup" {
		t.Fatalf("want say then hangup, got %+v then %+v", first, second)
	}
	select {
	case <-brain.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("End was not called when the conversation finished")
	}
}
