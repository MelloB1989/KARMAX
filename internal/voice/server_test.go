package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		ServeConversation(r.Context(), c, func(Call) Brain { return brain }, zap.NewNop())
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

// A brain that streams a reply sentence by sentence while it is still composing.
type streamBrain struct {
	testBrain
	sentences []string
	gap       time.Duration
}

func (b *streamBrain) Answer(ctx context.Context, u Utterance) (Reply, error) {
	say := SayFrom(ctx)
	for _, s := range b.sentences {
		if !say(s) {
			return Reply{}, nil
		}
		time.Sleep(b.gap)
	}
	return Reply{Hangup: strings.Contains(u.Text, "bye")}, nil
}

func TestStreamedSentencesArriveBeforeTheTurnEndsAndAreFlushed(t *testing.T) {
	brain := &streamBrain{testBrain: testBrain{notices: make(chan Reply, 1), ended: make(chan struct{})},
		sentences: []string{"One.", "Two.", "Three."}, gap: 150 * time.Millisecond}
	conn, done := dial(t, brain)
	defer done()
	send(t, conn, wire{Type: "start"})
	recv(t, conn, 2*time.Second) // greeting
	send(t, conn, wire{Type: "utterance", ID: 1, Text: "bye now"})
	var got []wire
	first := time.Now()
	for {
		m, ok := recv(t, conn, 2*time.Second)
		if !ok {
			t.Fatalf("stream ended early: %+v", got)
		}
		if len(got) == 0 && time.Since(first) > 300*time.Millisecond {
			t.Fatal("first sentence waited for the whole turn")
		}
		got = append(got, m)
		if m.Type == "hangup" {
			break
		}
	}
	var seq []string
	for _, m := range got {
		seq = append(seq, m.Type+":"+m.Text+":"+strconv.FormatBool(m.More))
	}
	want := []string{"say:One.:false", "say:Two.:true", "say:Three.:true", "flush::false", "hangup::false"}
	if strings.Join(seq, " ") != strings.Join(want, " ") {
		t.Fatalf("sequence = %v, want %v", seq, want)
	}
}

func TestSentencesStopOnceTheCallerMovesOn(t *testing.T) {
	brain := &streamBrain{testBrain: testBrain{notices: make(chan Reply, 1), ended: make(chan struct{})},
		sentences: []string{"One.", "Two.", "Three.", "Four."}, gap: 200 * time.Millisecond}
	conn, done := dial(t, brain)
	defer done()
	send(t, conn, wire{Type: "start"})
	recv(t, conn, 2*time.Second)
	send(t, conn, wire{Type: "utterance", ID: 1, Text: "hello"})
	recv(t, conn, 2*time.Second)
	send(t, conn, wire{Type: "utterance", ID: 2, Text: "wait, actually"})
	count := 0
	for {
		m, ok := recv(t, conn, 1500*time.Millisecond)
		if !ok {
			break
		}
		if m.For == 1 && m.Type == "say" {
			count++
		}
	}
	if count > 1 {
		t.Fatalf("%d more sentences of a stale reply were sent", count)
	}
}

func TestStartCarriesTheTagCapability(t *testing.T) {
	got := make(chan Call, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ServeConversation(r.Context(), c, func(call Call) Brain {
			got <- call
			return &testBrain{notices: make(chan Reply, 1), ended: make(chan struct{})}
		}, zap.NewNop())
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	send(t, conn, wire{Type: "start", TTSTags: true})
	if c := <-got; !c.Tags {
		t.Fatalf("call = %+v", c)
	}
}
