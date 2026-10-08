package reflex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MelloB1989/karma/ai/jev"
	"github.com/MelloB1989/karmax/internal/bus"
	"go.uber.org/zap"
)

// End to end against a stubbed TypeSafe, which is the only way to exercise the
// whole path: the sheet serialising, the request being accepted, the answers
// parsing, and the policy running on them.

func stubJev(t *testing.T, handler http.HandlerFunc) *Evaluator {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := jev.New(jev.WithAPIKey("test-key"),
		jev.WithBaseURL(srv.URL), jev.WithTimeout(2*time.Second),
		jev.WithRetry(jev.RetryPolicy{}))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return &Evaluator{
		client: client, thresholds: DefaultThresholds,
		timeout: 2 * time.Second, log: zap.NewNop(),
	}
}

// answers is the server's side of the sheet.
func answers(disposition string, confidence, urgency, risk, remember float64, effort string) []byte {
	body, _ := json.Marshal(jev.Result{
		Model: "jev-1.0.0",
		Usage: jev.Usage{InputTokens: 412},
		Answers: jev.Answers{
			QDisposition: {Type: jev.TypeChoice, Choice: disposition, Confidence: confidence,
				Probabilities: map[string]float64{disposition: confidence}},
			QUrgency:  {Type: jev.TypeScore, Score: urgency, Confidence: 0.9},
			QRisk:     {Type: jev.TypeScore, Score: risk, Confidence: 0.9},
			QRemember: {Type: jev.TypeNoul, Noul: remember},
			QEffort: {Type: jev.TypeChoice, Choice: effort, Confidence: 0.9,
				Probabilities: map[string]float64{effort: 0.9}},
		},
	})
	return body
}

func TestScreenEndToEnd(t *testing.T) {
	var gotBody []byte
	e := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = readAll(r)
		w.Header().Set("Content-Type", "application/json")
		w.Write(answers("drop", 0.94, 0, 0, 0.1, "trivial"))
	})

	evt := bus.NewEvent(bus.EventCommsMessage, "agent-1", map[string]any{
		"content": "👍", "channel_id": "1234",
	})
	v := e.Screen(context.Background(), evt, Hint{Operator: false, Sender: "A Group"})

	if v.FailedOpen {
		t.Fatalf("a good answer must not read as failed-open: %+v", v)
	}
	if v.Action != ActionDrop {
		t.Errorf("action = %q, want drop", v.Action)
	}
	if v.Model != "jev-1.0.0" {
		t.Errorf("model = %q, want the versioned id the server reported", v.Model)
	}
	if v.InputTokens != 412 {
		t.Errorf("input tokens = %d, want them carried for cost accounting", v.InputTokens)
	}
	if v.Elapsed == 0 {
		t.Error("elapsed should be measured")
	}

	// The request must actually carry the whole sheet and the event's content.
	var sent struct {
		State     map[string]any             `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if len(sent.Questions) != len(Sheet()) {
		t.Errorf("sent %d questions, want the whole sheet of %d", len(sent.Questions), len(Sheet()))
	}
	payload, _ := sent.State["payload"].(map[string]any)
	if payload["content"] != "👍" {
		t.Errorf("state did not carry the message: %v", sent.State)
	}

	if s := e.Stats(); s.Screened != 1 || s.Dropped != 1 || s.InputTokens != 412 {
		t.Errorf("stats = %+v", s)
	}
}

// The floor has to hold through the real path, not only in Decide.
func TestScreenFloorsTheOperatorEndToEnd(t *testing.T) {
	e := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(answers("drop", 0.99, 0, 0, 0.9, "trivial"))
	})
	evt := bus.NewEvent(bus.EventCommsMessage, "agent-1", map[string]any{"content": "ok"})
	v := e.Screen(context.Background(), evt, Hint{Operator: true})
	if v.Silent() {
		t.Fatalf("the operator was silenced end to end: %+v", v)
	}
}

func TestScreenFailsOpenOnServerError(t *testing.T) {
	e := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	})
	v := e.Screen(context.Background(), bus.NewEvent(bus.EventCommsMessage, "a", nil), Hint{})
	if v.Action != ActionHandle || !v.FailedOpen {
		t.Fatalf("a 500 must fail open, got %+v", v)
	}
	if e.Stats().Failed != 1 {
		t.Error("the failure should be counted")
	}
}

func TestScreenFailsOpenOnGarbage(t *testing.T) {
	e := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json at all`))
	})
	v := e.Screen(context.Background(), bus.NewEvent(bus.EventCommsMessage, "a", nil), Hint{})
	if !v.FailedOpen {
		t.Fatalf("an unreadable answer must fail open, got %+v", v)
	}
}

// The breaker is what stops an outage costing one timeout per event.
func TestBreakerStopsCallingAfterRepeatedFailures(t *testing.T) {
	var calls atomic.Int32
	e := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":{"message":"down"}}`, http.StatusInternalServerError)
	})
	evt := bus.NewEvent(bus.EventCommsMessage, "a", nil)
	for i := 0; i < breakerTrip+3; i++ {
		v := e.Screen(context.Background(), evt, Hint{})
		if !v.FailedOpen {
			t.Fatal("every one of these must fail open")
		}
	}
	if got := calls.Load(); got > breakerTrip {
		t.Errorf("made %d calls, want the breaker to stop at %d", got, breakerTrip)
	}
	if !e.Stats().BreakerOpen {
		t.Error("the breaker should read as open")
	}
	if e.Available() {
		t.Error("an open breaker means unavailable")
	}
}

// A kind filter must not spend a call to decide it is not interested.
func TestKindFilterSkipsWithoutCalling(t *testing.T) {
	var calls atomic.Int32
	e := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write(answers("drop", 0.99, 0, 0, 0, "trivial"))
	})
	e.kinds = map[string]bool{string(bus.EventTimerFired): true}

	v := e.Screen(context.Background(), bus.NewEvent(bus.EventCommsMessage, "a", nil), Hint{})
	if v.Action != ActionHandle || !v.FailedOpen {
		t.Errorf("an unscreened kind should pass through, got %+v", v)
	}
	if calls.Load() != 0 {
		t.Error("an unscreened kind must not cost a call")
	}
}

// Ask is the loops' path, and unavailability there must be an error rather
// than a fabricated answer.
func TestAskReturnsUnavailableWhenOff(t *testing.T) {
	var e *Evaluator
	if _, err := e.Ask(context.Background(), "x", jev.Questions{}); err != ErrUnavailable {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestAskRoundTrips(t *testing.T) {
	e := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(jev.Result{
			Model:   "jev-1.0.0",
			Answers: jev.Answers{"reply": {Type: jev.TypeNoul, Noul: 0.83}},
		})
		w.Write(body)
	})
	res, err := e.Ask(context.Background(), map[string]any{"msg": "hi"}, jev.Questions{
		"reply": jev.Noul("Reply?"),
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	p, err := res.Noul("reply")
	if err != nil || p != 0.83 {
		t.Errorf("noul = %v (%v), want 0.83", p, err)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}
