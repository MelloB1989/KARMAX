package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
)

// Two replies to one thread landing together used to run Append on the same
// conversation from two goroutines; its window is plain memory, and compaction
// sliced it past its length. Run with -race.
func TestConcurrentRecordsOnOneThreadAreSerialised(t *testing.T) {
	var seq atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		p := r.URL.Path
		switch {
		case p == "/v1/conversations" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{"branch": "main", "next_seq": 0})
		case strings.HasSuffix(p, "/messages"):
			n := int64(len(body["messages"].([]any)))
			_ = json.NewEncoder(w).Encode(map[string]any{"next_seq": seq.Add(n)})
		case strings.HasSuffix(p, "/compact"):
			_ = json.NewEncoder(w).Encode(map[string]any{"summary": "s"})
		default:
			http.Error(w, `{"error":{"code":"not_found","message":"no"}}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewConversations(ConversationsConfig{APIKey: "k", BaseURL: srv.URL, Namespace: "ns", Model: "gpt-4o"}, zap.NewNop())

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Long enough that the window overflows and compaction runs.
			c.Record(t.Context(), "chat:1", "t", strings.Repeat("question ", 4000)+fmt.Sprint(i), strings.Repeat("answer ", 4000))
		}(i)
	}
	wg.Wait()
}

func TestDifferentThreadsDoNotShareALock(t *testing.T) {
	c := &Conversations{locks: map[string]*sync.Mutex{}}
	if c.lockFor("a") == c.lockFor("b") {
		t.Fatal("two threads share one lock")
	}
	if c.lockFor("a") != c.lockFor("a") {
		t.Fatal("one thread got two locks")
	}
}
