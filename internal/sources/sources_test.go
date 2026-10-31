package sources

import (
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
)

// An event from any source reaches the agent the way a webhook does: one
// kind the router already delivers and the screener already reads, kept in
// order per source account.
func TestAnEventTravelsAsAWebhookFromItsSourceAccount(t *testing.T) {
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	e := Event{Source: "github", Account: "octo", Type: "pull_request.review_requested", Key: "n-1",
		Title: "Review requested: fix login", URL: "https://example.test/pr/1", At: at, Urgent: true,
		Data: map[string]any{"repo": "octo/app"}}

	got := e.BusEvent("agent-1")
	if got.Kind != bus.EventWebhookFired || got.AgentID != "agent-1" {
		t.Fatalf("kind %q to %q, want webhook.fired to the agent", got.Kind, got.AgentID)
	}
	if got.Payload["route"] != "github:octo" {
		t.Errorf("route = %v, want github:octo so one account's events stay in order", got.Payload["route"])
	}
	for k, want := range map[string]any{"source": "github", "account": "octo", "type": "pull_request.review_requested",
		"key": "n-1", "title": "Review requested: fix login", "url": "https://example.test/pr/1", "urgent": true} {
		if got.Payload[k] != want {
			t.Errorf("payload[%s] = %v, want %v", k, got.Payload[k], want)
		}
	}
	if got.Timestamp != at || got.Meta["source"] != "github" {
		t.Errorf("timestamp %v meta %v", got.Timestamp, got.Meta)
	}
}

// A source that polls sees the same item more than once; it is published once.
func TestSeenPublishesAnItemOnce(t *testing.T) {
	kv := map[string]string{}
	s := Seen{Group: "src-test", Get: func(g, k string) (string, bool, error) { v, ok := kv[g+"/"+k]; return v, ok, nil },
		Set: func(g, k, v string, _ time.Duration) error { kv[g+"/"+k] = v; return nil }}
	first, err := s.First("n-1")
	if err != nil || !first {
		t.Fatalf("first sighting = %v, %v", first, err)
	}
	if again, _ := s.First("n-1"); again {
		t.Error("the same item was taken as new twice")
	}
}
