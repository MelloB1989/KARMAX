package reflex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/sources"
)

func sourceBusEvent(t *testing.T, e sources.Event) bus.Event {
	t.Helper()
	// Through JSON, as the router reads it back off the bus log.
	wire, err := json.Marshal(e.BusEvent("agent-1"))
	if err != nil {
		t.Fatal(err)
	}
	var evt bus.Event
	if err := json.Unmarshal(wire, &evt); err != nil {
		t.Fatal(err)
	}
	return evt
}

// The screener judges an account's event on what a person would read: which
// service and account it came from, what kind of thing happened, and what it
// says — not on the router's plumbing.
func TestASourceEventIsScreenedOnWhatHappened(t *testing.T) {
	evt := sourceBusEvent(t, sources.Event{
		Source: "github", Account: "octo-work", Type: "review_requested", Key: "n-1",
		Title: "Review requested on acme/api#12", Summary: "Dana wants a review before the release.",
		URL: "https://example.test/acme/api/12", At: time.Now(), Urgent: true,
		Data: map[string]any{"repo": "acme/api"},
	})

	state := StateOf(evt, Hint{})

	for k, want := range map[string]any{
		"source": "github", "account": "octo-work", "type": "review_requested",
		"title": "Review requested on acme/api#12", "summary": "Dana wants a review before the release.",
		"urgent": true,
	} {
		if state[k] != want {
			t.Errorf("state[%s] = %v, want %v", k, state[k], want)
		}
	}
	if d, _ := state["details"].(map[string]any); d["repo"] != "acme/api" {
		t.Errorf("details = %v, want the source's data", state["details"])
	}
	// Routing ids say nothing about the decision and cost tokens on every event.
	if _, ok := state["payload"]; ok {
		t.Errorf("a source event was screened as a raw payload: %v", state["payload"])
	}
	encoded, _ := json.Marshal(state)
	if strings.Contains(string(encoded), `"key"`) || strings.Contains(string(encoded), `"route"`) {
		t.Errorf("plumbing reached the screener: %s", encoded)
	}
}

// A webhook's request headers can carry credentials, and the screener is a
// third-party API. They decide nothing about the event.
func TestWebhookHeadersNeverLeaveForTheScreener(t *testing.T) {
	evt := bus.NewEvent(bus.EventWebhookFired, "agent-1", map[string]any{
		"route": "/trigger", "method": "POST", "body": `{"deploy":"done"}`,
		"headers": map[string]any{"Authorization": "Bearer not-a-real-token"},
	})
	encoded, _ := json.Marshal(StateOf(evt, Hint{}))
	if strings.Contains(string(encoded), "not-a-real-token") || strings.Contains(string(encoded), "headers") {
		t.Errorf("headers reached the screener: %s", encoded)
	}
	if !strings.Contains(string(encoded), "deploy") {
		t.Errorf("the body was lost: %s", encoded)
	}
}

// A source marks what the operator must hear about now. That is not the
// screener's to overrule: an urgent event is never dropped or merely filed,
// and it is escalated whatever urgency the model put on it.
func TestAnUrgentEventIsNeverSilencedAndIsEscalated(t *testing.T) {
	for _, disposition := range []string{"drop", "remember", "handle"} {
		v := Decide(result(sheet{disposition: disposition, confidence: 0.97, urgency: 0, remember: 0.9}),
			Hint{Urgent: true}, DefaultThresholds)
		if v.Action != ActionEscalate {
			t.Errorf("%s on an urgent event became %q, want escalate", disposition, v.Action)
		}
	}
	// Not urgent: the model's verdict stands.
	if v := Decide(result(sheet{disposition: "drop", confidence: 0.97}), Hint{}, DefaultThresholds); v.Action != ActionDrop {
		t.Errorf("a non-urgent confident drop became %q", v.Action)
	}
}
