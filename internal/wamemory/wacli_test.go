package wamemory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

// fakeWacli answers /messages the way wacli does: the NEWEST `limit` messages
// inside the bounds, oldest first.
func fakeWacli(t *testing.T, msgs []time.Time) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/messages":
			q := r.URL.Query()
			after, _ := time.Parse(time.RFC3339, q.Get("after"))
			before, _ := time.Parse(time.RFC3339, q.Get("before"))
			var limit int
			fmt.Sscan(q.Get("limit"), &limit)
			var in []time.Time
			for _, at := range msgs {
				if at.After(after) && (before.IsZero() || at.Before(before)) {
					in = append(in, at)
				}
			}
			sort.Slice(in, func(i, j int) bool { return in[i].Before(in[j]) })
			if len(in) > limit {
				in = in[len(in)-limit:]
			}
			out := []map[string]any{}
			for _, at := range in {
				out = append(out, map[string]any{"id": at.Format("150405"), "chat_jid": q.Get("chat_ref"), "sender_jid": "1@lid",
					"content": "m " + at.Format("15:04"), "timestamp": at.Format(time.RFC3339), "is_from_me": false})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": out})
		case "/chats":
			_ = json.NewEncoder(w).Encode(map[string]any{"chats": []map[string]any{
				{"jid": "1@g.us", "name": "Team", "is_group": true, "last_message_at": "2026-10-30T21:41:45+05:30"}}})
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

	got, err := src.Messages(context.Background(), "1@g.us", start.Add(-time.Second), 20)
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

	got, err := src.Messages(context.Background(), "1@g.us", start, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].At.Equal(later) {
		t.Fatalf("got %v, want the two messages after forty quiet days", got)
	}
}

func TestNoMessagesAfterTheLastOne(t *testing.T) {
	start := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	src := NewWacli(fakeWacli(t, []time.Time{start}).URL)
	src.now = func() time.Time { return start.Add(time.Hour) }

	got, err := src.Messages(context.Background(), "1@g.us", start, 20)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want nothing", got, err)
	}
}

func TestChatsAndNames(t *testing.T) {
	src := NewWacli(fakeWacli(t, nil).URL)
	chats, err := src.Chats(context.Background())
	if err != nil || len(chats) != 1 || chats[0].Name != "Team" || !chats[0].IsGroup || chats[0].LastMessageAt.IsZero() {
		t.Fatalf("chats = %+v, %v", chats, err)
	}
	if n := src.Name(context.Background(), "777:4@lid"); n != "Priya Raman" {
		t.Errorf("name = %q", n)
	}
}

func first(ms []Message) any {
	if len(ms) == 0 {
		return nil
	}
	return ms[0].At
}
