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
	// Backlog marks an item published on first sight of an account: it
	// happened before KARMAX was watching, so it is context, not news, and it
	// never pages the operator however urgent it is.
	Backlog bool
	// MemorySynced marks an event from a conversation a memory syncer already
	// reads into long-term memory (see MemorySync): the turn acts on it, and
	// leaves filing what it says to the syncer, so memory holds the
	// conversation once.
	MemorySynced bool
	Data         map[string]any
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
	if e.Backlog {
		ev.Payload["backlog"] = true
	}
	if e.MemorySynced {
		ev.Payload[memorySyncedKey] = true
	}
	if !e.At.IsZero() {
		ev.Timestamp = e.At
	}
	ev.Meta = map[string]string{"source": e.Source, "account": e.Account}
	return ev
}

// WatchedSince is the event as seen by KARMAX watching its account since
// firstLook: marked backlog when it happened at or before then. An undated
// event cannot be placed and stays news; a zero firstLook marks nothing; and
// an event its source already marked backlog keeps the mark.
func (e Event) WatchedSince(firstLook time.Time) Event {
	if !firstLook.IsZero() && !e.At.IsZero() && !e.At.After(firstLook) {
		e.Backlog = true
	}
	return e
}

// FirstLook remembers when KARMAX first looked at each account of a source,
// so a polling source can tell what its first read found — which happened
// before KARMAX was watching, and is backlog — from what happened since. The
// time is recorded on the first ask and kept, so a restart does not move it.
type FirstLook struct {
	Group string
	Get   func(group, key string) (string, bool, error)
	Set   func(group, key, value string, ttl time.Duration) error
}

// Since returns when KARMAX first looked at account, recording now (and
// reporting first) when it never has. A record that cannot be read counts as
// none: a first look again marks a lookback's worth as backlog, which is
// better than an account whose events can never be placed.
func (f FirstLook) Since(account string, now time.Time) (since time.Time, first bool, err error) {
	v, ok, err := f.Get(f.Group, account)
	if err != nil {
		return time.Time{}, false, err
	}
	if ok {
		if t, perr := time.Parse(time.RFC3339Nano, v); perr == nil {
			return t, false, nil
		}
	}
	return now, true, f.Set(f.Group, account, now.UTC().Format(time.RFC3339Nano), 0)
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
