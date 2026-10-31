package chatmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap"
)

// wacliMsg is a message as the fake wacli holds it.
type wacliMsg struct {
	id, text string
	at       time.Time
}

// fakeWacli holds one message at each time, each with an id and text of its
// own.
func fakeWacli(t *testing.T, msgs []time.Time) *httptest.Server {
	t.Helper()
	held := make([]wacliMsg, 0, len(msgs))
	for _, at := range msgs {
		held = append(held, wacliMsg{id: at.Format("0102150405"), text: "m " + at.Format("15:04"), at: at})
	}
	return fakeWacliOf(t, held)
}

// fakeWacliOf answers /messages the way wacli's store does: timestamps to the
// second, both bounds included (timestamp >= after AND timestamp <= before),
// and the NEWEST `limit` messages inside them, oldest first.
func fakeWacliOf(t *testing.T, msgs []wacliMsg) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/messages":
			q := r.URL.Query()
			after, _ := time.Parse(time.RFC3339, q.Get("after"))
			before, _ := time.Parse(time.RFC3339, q.Get("before"))
			var limit int
			fmt.Sscan(q.Get("limit"), &limit)
			var in []wacliMsg
			for _, m := range msgs {
				at := m.at.Truncate(time.Second)
				if !at.Before(after) && (before.IsZero() || !at.After(before)) {
					in = append(in, m)
				}
			}
			sort.SliceStable(in, func(i, j int) bool { return in[i].at.Before(in[j].at) })
			if len(in) > limit {
				in = in[len(in)-limit:]
			}
			out := []map[string]any{}
			for _, m := range in {
				out = append(out, map[string]any{"id": m.id, "chat_jid": q.Get("chat_ref"), "sender_jid": "1@lid",
					"content": m.text, "timestamp": m.at.Format(time.RFC3339), "is_from_me": false})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": out})
		case "/chats":
			_ = json.NewEncoder(w).Encode(map[string]any{"chats": []map[string]any{
				{"jid": "1@g.us", "name": "Team", "is_group": true, "last_message_at": "2026-10-30T21:41:45+05:30"},
				{"jid": "3@newsletter", "name": "News", "is_group": false, "last_message_at": "2026-10-30T21:41:45+05:30"},
				{"jid": "status@broadcast", "name": "", "is_group": false, "last_message_at": "2026-10-30T21:41:45+05:30"},
				{"jid": "919990001111@s.whatsapp.net", "name": "", "is_group": false, "last_message_at": "2026-10-30T20:00:00+05:30"}}})
		case "/resolve":
			_ = json.NewEncoder(w).Encode(map[string]any{"matches": []map[string]any{
				{"jid": "777@lid", "name": "Priya Raman", "match_type": "jid_exact"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Asked for what follows a point in a long history, wacli's newest-first limit
// would answer with today's messages and skip everything between.
func TestMessagesAreTheOldestAfterTheCursor(t *testing.T) {
	start := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	var msgs []time.Time
	for i := 0; i < 500; i++ {
		msgs = append(msgs, start.Add(time.Duration(i)*37*time.Minute))
	}
	src := NewWacli(fakeWacli(t, msgs).URL)
	src.now = func() time.Time { return start.Add(500 * 37 * time.Minute) }

	got, err := src.Messages(context.Background(), Chat{ID: "1@g.us"}, Cursor{At: start.Add(-time.Second)}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || !got[0].At.Equal(start) {
		t.Fatalf("first message = %v, want the first one after the cursor (%v)", first(got), start)
	}
	for i := 1; i < len(got); i++ {
		if !got[i].At.Equal(got[i-1].At.Add(37 * time.Minute)) {
			t.Fatalf("a gap between %v and %v: messages were skipped", got[i-1].At, got[i].At)
		}
	}
}

func TestMessagesAcrossAQuietStretch(t *testing.T) {
	start := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	later := start.Add(40 * 24 * time.Hour)
	src := NewWacli(fakeWacli(t, []time.Time{later, later.Add(time.Minute)}).URL)
	src.now = func() time.Time { return later.Add(time.Hour) }

	got, err := src.Messages(context.Background(), Chat{ID: "1@g.us"}, Cursor{At: start}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].At.Equal(later) {
		t.Fatalf("got %v, want the two messages after forty quiet days", got)
	}
}

// A cursor on a chat's last message reads nothing, though wacli's bounds
// include that message's second.
func TestNoMessagesAfterTheLastOne(t *testing.T) {
	start := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	src := NewWacli(fakeWacli(t, []time.Time{start}).URL)
	src.now = func() time.Time { return start.Add(time.Hour) }

	got, err := src.Messages(context.Background(), Chat{ID: "1@g.us"}, Cursor{At: start, ID: start.Format("0102150405")}, 20)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want nothing", got, err)
	}
}

// Read from a cursor that stands between messages of one second, wacli
// answers with the rest of that second, in order, and then what follows; a
// stretch holding only messages the cursor has passed is looked past.
func TestWacliReadsOnFromTheCursorsOwnMessage(t *testing.T) {
	sec := time.Date(2026, 8, 20, 22, 7, 46, 0, time.UTC)
	later := sec.Add(20 * 24 * time.Hour)
	src := NewWacli(fakeWacliOf(t, []wacliMsg{
		{id: "3EB0C3", text: "c", at: sec},
		{id: "3EB0A1", text: "a", at: sec},
		{id: "3EB0B2", text: "b", at: sec},
		{id: "3EB0D4", text: "d", at: later},
	}).URL)
	src.now = func() time.Time { return later.Add(time.Hour) }
	ctx := context.Background()

	got, err := src.Messages(ctx, Chat{ID: "1@g.us"}, Cursor{At: sec, ID: "3EB0A1"}, 20)
	if err != nil || len(got) != 2 || got[0].ID != "3EB0B2" || got[1].ID != "3EB0C3" {
		t.Fatalf("got %v, %v; want the rest of the cursor's second, in order", ids(got), err)
	}
	got, err = src.Messages(ctx, Chat{ID: "1@g.us"}, Cursor{At: sec, ID: "3EB0C3"}, 20)
	if err != nil || len(got) != 1 || got[0].ID != "3EB0D4" {
		t.Fatalf("got %v, %v; want the message twenty days on", ids(got), err)
	}
}

func ids(ms []Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// Channels and status updates are broadcast at the operator, not said to
// them, so they are not listed; a direct chat with no name is named by its
// number.
func TestChatsAndNames(t *testing.T) {
	src := NewWacli(fakeWacli(t, nil).URL)
	chats, err := src.Chats(context.Background())
	if err != nil || len(chats) != 2 {
		t.Fatalf("chats = %+v, %v; want the group and the direct chat", chats, err)
	}
	if c := chats[0]; c.Name != "Team" || !c.IsGroup || c.LastMessageAt.IsZero() {
		t.Errorf("group = %+v", c)
	}
	if c := chats[1]; c.Name != "+919990001111" || c.IsGroup {
		t.Errorf("direct chat = %+v; want it named by its number", c)
	}
	if n := src.Name(context.Background(), "777:4@lid"); n != "Priya Raman" {
		t.Errorf("name = %q", n)
	}
}

// wacliChats is the real wacli reader, listing the chats it is given.
type wacliChats struct {
	*Wacli
	chats []Chat
}

func (w wacliChats) Chats(context.Context) ([]Chat, error) { return w.chats, nil }

// wacliRun is a syncer reading one chat through the real wacli reader, from a
// fake wacli that holds msgs.
type wacliRun struct {
	s     *Syncer
	ask   *recorder
	cur   memCursors
	saves []saved
}

func newWacliRun(t *testing.T, chat Chat, since, now time.Time, msgs ...wacliMsg) *wacliRun {
	t.Helper()
	src := NewWacli(fakeWacliOf(t, msgs).URL)
	src.now = func() time.Time { return now }
	r := &wacliRun{ask: &recorder{}, cur: memCursors{}}
	r.s = New(Config{Name: "wa-memory", Service: "WhatsApp", Tag: "whatsapp", Operator: "Quinn", Since: since, Location: time.UTC},
		wacliChats{src, []Chat{chat}}, &fakeMemory{}, loggedCursors{r.cur, &r.saves}, nil, r.ask.ask, zap.NewNop())
	return r
}

func (r *wacliRun) ticks(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// Live, a group's cursor stood on one of two messages of the same second —
// a sticker and a photo, nothing to read — with nothing else said for weeks.
// wacli was read from the cursor's second, its first window held only those
// messages, and the chat was taken as read to the end: what was said weeks
// later was never reached.
func TestAStretchOfMessagesAlreadyReadDoesNotEndTheChat(t *testing.T) {
	sec := time.Date(2026, 8, 20, 22, 7, 45, 0, time.UTC)
	later := sec.Add(12 * 24 * time.Hour)
	msgs := []wacliMsg{
		{id: "3EB0AA", text: "see you all there", at: sec},
		{id: "3EB0CC", at: sec.Add(time.Second)},
		{id: "3EB0BB", at: sec.Add(time.Second)},
		{id: "3EB0DD", text: "the venue moved to the north hall", at: later},
		{id: "3EB0EE", text: "thanks for the heads up", at: later.Add(time.Minute)},
	}
	// The cursor flipped between the two; it may have been saved on either.
	for _, on := range []string{"3EB0BB", "3EB0CC"} {
		t.Run("on "+on, func(t *testing.T) {
			chat := Chat{ID: "1@g.us", Name: "Team", IsGroup: true, LastMessageAt: later.Add(time.Minute)}
			r := newWacliRun(t, chat, sec.Add(-time.Hour), later.Add(time.Hour), msgs...)
			r.cur["1@g.us"] = Cursor{At: sec.Add(time.Second), ID: on}
			r.saves = append(r.saves, saved{"1@g.us", r.cur["1@g.us"]})

			r.ticks(t, 3)
			for _, text := range []string{"the venue moved to the north hall", "thanks for the heads up"} {
				if n := timesRead(r.ask.prompts, text); n != 1 {
					t.Errorf("%q was read %d times, want once", text, n)
				}
			}
			if n := timesRead(r.ask.prompts, "see you all there"); n != 0 {
				t.Errorf("a message behind the cursor was read %d times", n)
			}
			if c := r.cur["1@g.us"]; c.ID != "3EB0EE" {
				t.Errorf("cursor = %v|%s, want it on the last message", c.At.Format(time.RFC3339), c.ID)
			}
			cursorsOnlyMovedForward(t, r.saves)
		})
	}
}

// A chat quiet for longer than wacli's first window after the message its
// cursor stands on: what is said after the quiet is still read. The first
// window from the cursor held only the cursor's own message, nothing in it
// was new, and the cursor jumped to the chat's last activity, skipping all
// that lay between.
func TestAQuietStretchAfterTheCursorSkipsNothing(t *testing.T) {
	start := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	later := start.Add(10 * 24 * time.Hour)
	chat := Chat{ID: "1@g.us", Name: "Team", IsGroup: true, LastMessageAt: later.Add(time.Minute)}
	r := newWacliRun(t, chat, start.Add(-time.Hour), later.Add(time.Hour),
		wacliMsg{id: "3EB0A1", text: "kickoff is on monday", at: start},
		wacliMsg{id: "3EB0B2", text: "kickoff moved to thursday", at: later},
		wacliMsg{id: "3EB0C3", text: "thursday works for me", at: later.Add(time.Minute)})

	r.ticks(t, 3)
	for _, text := range []string{"kickoff is on monday", "kickoff moved to thursday", "thursday works for me"} {
		if n := timesRead(r.ask.prompts, text); n != 1 {
			t.Errorf("%q was read %d times, want once", text, n)
		}
	}
	if c := r.cur["1@g.us"]; c.ID != "3EB0C3" {
		t.Errorf("cursor = %v|%s, want it on the last message", c.At.Format(time.RFC3339), c.ID)
	}
	cursorsOnlyMovedForward(t, r.saves)
}

func first(ms []Message) any {
	if len(ms) == 0 {
		return nil
	}
	return ms[0].At
}
