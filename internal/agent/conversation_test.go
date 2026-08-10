package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MelloB1989/karmax/internal/bus"
	"go.uber.org/zap"
)

// Third parties are not archived.
//
// KARMAX watches WhatsApp chats it holds on the operator's behalf. Those are
// conversations with people who never agreed to be transcribed, and the
// memories drawn from them already answer everything a transcript would. If
// this boundary slips, KARMAX quietly starts keeping a record of everyone the
// operator talks to.
func TestOnlyTheOperatorsConversationsAreArchived(t *testing.T) {
	a := &Agent{def: AgentDef{ID: "nexus"}, log: zap.NewNop()}
	a.SetOperatorChats([]string{"919999999999"})

	for _, tc := range []struct {
		name     string
		evt      bus.Event
		archived bool
	}{
		{
			name: "the operator's own chat",
			evt: bus.Event{Kind: bus.EventCommsMessage, Payload: map[string]any{
				"channel_id": "919999999999@s.whatsapp.net", "chat_name": "Me"}},
			archived: true,
		},
		{
			name: "a monitored third-party chat",
			evt: bus.Event{Kind: bus.EventCommsMessage, Payload: map[string]any{
				"channel_id": "918888888888@s.whatsapp.net", "chat_name": "Srikanth"}},
			archived: false,
		},
		{
			name:     "the phone app, which only the operator has",
			evt:      bus.Event{Kind: "api.chat", Payload: map[string]any{"content": "hi"}},
			archived: true,
		},
		{
			name:     "a loop firing, which is not a conversation at all",
			evt:      bus.Event{Kind: bus.EventDelegationDone, Payload: map[string]any{"job_id": "x"}},
			archived: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok := a.conversationThread(tc.evt)
			if ok != tc.archived {
				t.Errorf("archived = %v, want %v", ok, tc.archived)
			}
		})
	}
}

// The operator's WhatsApp thread and their app thread are separate
// conversations. Merging them produces a transcript that reads as neither.
func TestEachChannelThreadIsItsOwnConversation(t *testing.T) {
	a := &Agent{def: AgentDef{ID: "nexus"}, log: zap.NewNop()}
	a.SetOperatorChats([]string{"919999999999"})

	wa, _, _ := a.conversationThread(bus.Event{Kind: bus.EventCommsMessage,
		Payload: map[string]any{"channel_id": "919999999999@s.whatsapp.net"}})
	app, _, _ := a.conversationThread(bus.Event{Kind: "api.chat"})

	if wa == app {
		t.Fatalf("both channels resolved to the same conversation %q", wa)
	}

	// And the same chat reached by a different JID form is the SAME thread, or
	// a device suffix would fork the operator's history in two.
	alt, _, _ := a.conversationThread(bus.Event{Kind: bus.EventCommsMessage,
		Payload: map[string]any{"channel_id": "919999999999:12@s.whatsapp.net"}})
	if alt != wa {
		t.Errorf("the same chat produced two threads: %q and %q", wa, alt)
	}
}

// Archiving is best-effort: it is the record, not the reply path, and a
// GitLoom that is down must never cost the operator their answer.
func TestArchivingFailureDoesNotBreakTheTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewConversations(ConversationsConfig{
		APIKey: "k", BaseURL: srv.URL, Namespace: "test",
	}, zap.NewNop())
	if c == nil {
		t.Fatal("a configured recorder should not be nil")
	}
	// The assertion is that this returns at all rather than panicking or
	// propagating; Record has no error to return by design.
	c.Record(context.Background(), "chat:1", "Test", "hello", "hi there")
}

// With no GitLoom configured the recorder is nil, and a nil recorder archives
// nothing without anybody having to check first.
func TestNoGitLoomMeansNoRecorder(t *testing.T) {
	if c := NewConversations(ConversationsConfig{Namespace: "test"}, zap.NewNop()); c != nil {
		t.Fatal("a recorder was built with no API key")
	}
	var c *Conversations
	c.Record(context.Background(), "chat:1", "Test", "hello", "hi")
}

// A thread that fails to open is not retried on every single message.
func TestABrokenThreadIsNotRetriedOnEveryMessage(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		http.Error(w, `{"error":"nope"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewConversations(ConversationsConfig{APIKey: "k", BaseURL: srv.URL, Namespace: "test"}, zap.NewNop())
	for i := 0; i < 5; i++ {
		c.Record(context.Background(), "chat:1", "Test", "hello", "hi")
	}

	mu.Lock()
	defer mu.Unlock()
	// One open attempt is a load followed by a create; five messages must not
	// become ten round trips to a store that is plainly not answering.
	if attempts > 2 {
		t.Errorf("%d round trips for five messages to a failing store", attempts)
	}
}

// What is stored is the exchange, in order, with roles intact.
func TestAnExchangeIsStoredAsUserThenAssistant(t *testing.T) {
	var mu sync.Mutex
	var appended []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages"):
			var body struct {
				Messages []map[string]any `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			appended = append(appended, body.Messages...)
			mu.Unlock()
			writeJSON(w, map[string]any{"next_seq": len(appended) + 1})
		case r.Method == http.MethodGet:
			writeJSON(w, map[string]any{"branch": "main", "next_seq": 1, "messages": []any{}})
		default:
			writeJSON(w, map[string]any{"branch": "main", "next_seq": 1})
		}
	}))
	defer srv.Close()

	c := NewConversations(ConversationsConfig{APIKey: "k", BaseURL: srv.URL, Namespace: "test"}, zap.NewNop())
	c.Record(context.Background(), "chat:1", "Test", "what did we agree", "three instalments")

	mu.Lock()
	defer mu.Unlock()
	if len(appended) != 2 {
		t.Fatalf("stored %d messages, want the user's and the reply", len(appended))
	}
	if role, _ := appended[0]["role"].(string); !strings.EqualFold(role, "user") {
		t.Errorf("first stored message has role %q, want user", role)
	}
	if role, _ := appended[1]["role"].(string); !strings.EqualFold(role, "assistant") {
		t.Errorf("second stored message has role %q, want assistant", role)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
