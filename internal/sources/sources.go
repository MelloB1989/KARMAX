// Package sources is the shape every event source shares: GitHub, Google
// Calendar and Chat, Slack, AWS, PostHog and the rest each turn what happened
// on one account into an Event, and the runtime publishes it.
//
// An Event travels as webhook.fired, the kind the agent router already
// delivers and reflex already screens, so a new source needs no new routing:
// it is screened, remembered and acted on like any webhook. Its route is
// "source:account", so one account's events stay in order while different
// accounts run side by side.
package sources

import (
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
)

// Event is one thing that happened on a source account.
type Event struct {
	Source  string // "github", "gcal", "gchat", "slack", "aws", "posthog", …
	Account string // which login it came from: a GitHub user, a Google address, an AWS profile
	Type    string // the source's own name for what happened
	Key     string // stable id of the item, for deduplication
	Title   string // one line a person would read
	Summary string // the detail worth deciding on
	URL     string
	At      time.Time
	// Urgent marks what the operator must hear about now: an alarm firing, a
	// bill spike, a person asking them directly.
	Urgent bool
	Data   map[string]any
}

// BusEvent is the event as the agent router delivers it.
func (e Event) BusEvent(agentID string) bus.Event {
	ev := bus.NewEvent(bus.EventWebhookFired, agentID, map[string]any{
		"route":   e.Source + ":" + e.Account,
		"source":  e.Source,
		"account": e.Account,
		"type":    e.Type,
		"key":     e.Key,
		"title":   e.Title,
		"summary": e.Summary,
		"url":     e.URL,
		"urgent":  e.Urgent,
		"data":    e.Data,
	})
	if !e.At.IsZero() {
		ev.Timestamp = e.At
	}
	ev.Meta = map[string]string{"source": e.Source, "account": e.Account}
	return ev
}

// Seen remembers which items a polling source has already published.
type Seen struct {
	Group string
	TTL   time.Duration // how long a key is remembered; 0 keeps it
	Get   func(group, key string) (string, bool, error)
	Set   func(group, key, value string, ttl time.Duration) error
}

// First reports whether key is new, and marks it seen.
func (s Seen) First(key string) (bool, error) {
	if _, ok, err := s.Get(s.Group, key); err != nil || ok {
		return false, err
	}
	return true, s.Set(s.Group, key, "1", s.TTL)
}
