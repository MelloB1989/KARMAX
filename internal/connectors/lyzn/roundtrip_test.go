package lyzn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// lyznStub is LYZN's daemon API with the parts that matter kept honest: a
// pairing code that is spent by being used, a bearer token that has to be the
// one handed back, a queue a task leaves when it is claimed, and a result that
// prints one receipt however many times it is posted.
//
// It exists because every other test in this package proves one call. The
// thing worth proving is the sequence: pair, beat, take, do, report — which is
// the whole of what a paired laptop ever does.
type lyznStub struct {
	mu       sync.Mutex
	code     string
	token    string
	queue    []map[string]any
	held     map[string]bool
	receipts map[string]string
	seq      int
	calls    []string
}

func newStub(code string, tasks ...string) *lyznStub {
	s := &lyznStub{code: code, held: map[string]bool{}, receipts: map[string]string{}}
	for _, id := range tasks {
		s.queue = append(s.queue, map[string]any{
			"taskId": id,
			"text":   "send the quote to Rahul",
			"kind":   "message",
			"quote":  "I'll send it tonight",
			"context": map[string]any{
				"title":   "Pricing call",
				"summary": "Agreed the number and who sends what",
				"facts":   []map[string]string{{"text": "Rahul prefers PDF", "kind": "preference"}},
			},
		})
	}
	return s
}

func (s *lyznStub) authed(r *http.Request) bool {
	return s.token != "" && r.Header.Get("Authorization") == "Bearer "+s.token
}

func (s *lyznStub) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)

		if r.URL.Path == "/daemons/claim" {
			var in struct{ Code string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			if s.code == "" || in.Code != s.code {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"that pairing code is not one we are waiting for"}`))
				return
			}
			s.code = "" // spent by being used
			s.token = "issued-token"
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"daemonId": "d-77", "token": s.token, "name": "studio",
			})
			return
		}

		if !s.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"this token is not paired to an account"}`))
			return
		}

		switch {
		case r.URL.Path == "/daemons/heartbeat":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "tasks": len(s.queue)})

		case r.URL.Path == "/daemons/work":
			_ = json.NewEncoder(w).Encode(map[string]any{"tasks": s.queue})

		case strings.HasSuffix(r.URL.Path, "/claim"):
			id := strings.Split(r.URL.Path, "/")[3]
			kept := s.queue[:0]
			var taken map[string]any
			for _, task := range s.queue {
				if task["taskId"] == id {
					taken = task
					continue
				}
				kept = append(kept, task)
			}
			if taken == nil {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"this task is not waiting to be claimed"}`))
				return
			}
			s.queue = kept
			s.held[id] = true
			_ = json.NewEncoder(w).Encode(map[string]any{"task": taken, "work": taken})

		case strings.HasSuffix(r.URL.Path, "/result"):
			id := strings.Split(r.URL.Path, "/")[3]
			if !s.held[id] {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"that task was never claimed"}`))
				return
			}
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			// One receipt per promise, however many times it is posted.
			if _, printed := s.receipts[id]; !printed {
				s.seq++
				s.receipts[id] = "rcp-" + string(rune('a'+s.seq-1))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task":    map[string]any{"taskId": id, "status": "done"},
				"receipt": map[string]any{"receiptId": s.receipts[id], "summary": in["summary"]},
			})

		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestTheWholeSequenceFromSixCharactersToAReceipt(t *testing.T) {
	stub := newStub("K7QD2M", "t-1")
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	ctx := context.Background()
	c := New()

	// 1. Somebody reads six characters off a phone and types them in.
	unpaired := connectorkit.Credentials{Config: map[string]string{
		keyAPI: srv.URL, keyCode: "k7q d2m",
	}}
	if err := c.ValidateCredentials(unpaired); err != nil {
		t.Fatalf("a code typed the way people type it was refused: %v", err)
	}
	filled, err := c.CompleteCredentials(ctx, unpaired)
	if err != nil {
		t.Fatal(err)
	}
	paired := connectorkit.Credentials{Config: map[string]string{
		keyAPI: srv.URL, keyToken: filled[keyToken], keyDaemonID: filled[keyDaemonID],
	}}

	// 2. The health check is a heartbeat, so the app can say ONLINE.
	if err := c.Health(ctx, paired); err != nil {
		t.Fatalf("a freshly paired machine reported itself unwell: %v", err)
	}

	// 3. The poll announces the promise once.
	events, cursor, err := pollWork(ctx, paired, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0]["task_id"] != "t-1" {
		t.Fatalf("the approved task was not announced: %v", events)
	}
	if events[0]["summary"] != "Agreed the number and who sends what" {
		t.Fatal("the conversation did not travel with the task, so the agent has a sentence with no context")
	}

	// 4. The agent takes it.
	claimed, err := claimWork(ctx, paired, map[string]any{"task_id": "t-1"})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.(map[string]any)["lease"] != "15m" {
		t.Fatal("the claim did not say it was a loan")
	}

	// A claimed task has left the queue, so the next poll has nothing to say.
	events, _, err = pollWork(ctx, paired, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("a task in hand was announced again: %v", events)
	}

	// 5. It reports, and a receipt is printed.
	out, err := reportWork(ctx, paired, map[string]any{
		"task_id": "t-1", "outcome": "done", "summary": "Sent the quote to Rahul.",
		"artifacts": []any{map[string]any{"name": "quote-RK-0904.pdf", "uri": "file:///tmp/q.pdf"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt := out.(map[string]any)["receipt"].(map[string]any)
	if receipt["receiptId"] != "rcp-a" {
		t.Fatalf("no receipt came back: %v", out)
	}

	// 6. A result posted twice — a reply lost on the way back — answers with
	// the receipt the first one printed, not a second one for one promise.
	again, err := reportWork(ctx, paired, map[string]any{
		"task_id": "t-1", "outcome": "done", "summary": "Sent the quote to Rahul.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.(map[string]any)["receipt"].(map[string]any)["receiptId"] != "rcp-a" {
		t.Fatal("a second receipt was printed for one promise")
	}

	// And the code cannot be spent twice.
	if _, err := c.CompleteCredentials(ctx, unpaired); err == nil {
		t.Fatal("a spent pairing code was accepted a second time")
	}
}

func TestAnUnpairedMachineGetsNoFurtherThanTheDoor(t *testing.T) {
	stub := newStub("K7QD2M")
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()

	// No token: the poll must fail loudly rather than quietly reporting an
	// empty queue, which would look exactly like "nothing to do".
	_, _, err := pollWork(context.Background(),
		connectorkit.Credentials{Config: map[string]string{keyAPI: srv.URL}}, "")
	if err == nil {
		t.Fatal("an unpaired machine polled successfully")
	}
	if !strings.Contains(err.Error(), "not paired") {
		t.Fatalf("the failure does not name the cause: %v", err)
	}
}
