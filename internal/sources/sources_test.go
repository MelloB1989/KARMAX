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

// Any polling source can ask when KARMAX first looked at an account: the time
// is recorded on the first ask and kept, so a restart does not move it.
func TestFirstLookIsRecordedOnceAndKept(t *testing.T) {
	kv := map[string]string{}
	f := FirstLook{Group: "src-first", Get: func(g, k string) (string, bool, error) { v, ok := kv[g+"/"+k]; return v, ok, nil },
		Set: func(g, k, v string, _ time.Duration) error { kv[g+"/"+k] = v; return nil }}
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

	since, first, err := f.Since("acct-1", at)
	if err != nil || !first || !since.Equal(at) {
		t.Fatalf("first ask = %v, %v, %v; want the time asked, as a first look", since, first, err)
	}
	since, first, err = f.Since("acct-1", at.Add(time.Hour))
	if err != nil || first || !since.Equal(at) {
		t.Errorf("second ask = %v, %v, %v; want the first look's time kept", since, first, err)
	}
	if other, first, _ := f.Since("acct-2", at.Add(time.Hour)); !first || !other.Equal(at.Add(time.Hour)) {
		t.Errorf("another account shares the first look: %v %v", other, first)
	}
	// Unreadable state is a first look again, rather than an account that
	// never polls.
	kv["src-first/acct-3"] = "not a time"
	if got, first, err := f.Since("acct-3", at); err != nil || !first || !got.Equal(at) {
		t.Errorf("unreadable first look = %v, %v, %v", got, first, err)
	}
}

// What happened at or before KARMAX's first look at an account is backlog;
// what happened after is news. Undated items cannot be placed and are news.
func TestAnItemFromBeforeTheFirstLookIsBacklog(t *testing.T) {
	since := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	if e := (Event{At: since.Add(-time.Minute), Urgent: true}).WatchedSince(since); !e.Backlog {
		t.Error("an item from before the first look was not marked backlog")
	}
	if e := (Event{At: since}).WatchedSince(since); !e.Backlog {
		t.Error("an item from the moment of the first look was not marked backlog")
	}
	if e := (Event{At: since.Add(time.Minute)}).WatchedSince(since); e.Backlog {
		t.Error("an item from after the first look was marked backlog")
	}
	if e := (Event{}).WatchedSince(since); e.Backlog {
		t.Error("an undated item was marked backlog")
	}
	if e := (Event{At: since.Add(-time.Minute)}).WatchedSince(time.Time{}); e.Backlog {
		t.Error("with no first look known, nothing is backlog")
	}
	if e := (Event{At: since.Add(time.Minute), Backlog: true}).WatchedSince(since); !e.Backlog {
		t.Error("an item its source already marked backlog lost the mark")
	}
}
