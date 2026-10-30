package sources

import (
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
)

// Reading an Event back off the bus.
//
// The bus log stores a payload as JSON, so what the router, the screener and
// the agent read is a generic map, not the Event a source published. Every
// consumer decodes it here, once, rather than each guessing at keys.

// FromBusEvent recognises a source event and decodes it. A webhook from the
// HTTP server is webhook.fired too, but carries no source and account, so it is
// not one.
func FromBusEvent(evt bus.Event) (Event, bool) {
	if evt.Kind != bus.EventWebhookFired || evt.Payload == nil {
		return Event{}, false
	}
	str := func(k string) string { s, _ := evt.Payload[k].(string); return strings.TrimSpace(s) }
	e := Event{
		Source:  str("source"),
		Account: str("account"),
		Type:    str("type"),
		Key:     str("key"),
		Title:   str("title"),
		Summary: str("summary"),
		URL:     str("url"),
		At:      evt.Timestamp,
	}
	if e.Source == "" || e.Account == "" {
		return Event{}, false
	}
	e.Urgent, _ = evt.Payload["urgent"].(bool)
	if d, ok := evt.Payload["data"].(map[string]any); ok {
		e.Data = d
	}
	return e, true
}

// Ref names the account an event came from, as "source:account" — the same
// string the event's route carries.
func (e Event) Ref() string { return e.Source + ":" + e.Account }

// subjectKeys are the Data keys that name what an event is about, best first.
// A source that knows better sets Data["subject"].
var subjectKeys = []string{
	"subject", "project", "repo", "person", "from", "sender", "author",
	"channel", "space", "calendar", "resource",
}

// Subject is what a fact filed from this event is ABOUT: the repository, the
// person, the project. Long-term memory keeps one file per subject, so this is
// what keeps an account's facts beside everything else known about the same
// thing instead of in one heap per source.
func (e Event) Subject() string {
	for _, k := range subjectKeys {
		if s, _ := e.Data[k].(string); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return strings.TrimSpace(e.Source + " " + e.Account)
}

// knownCategories are the categories long-term memory files by.
var knownCategories = map[string]bool{
	"relationship": true, "people": true, "person": true,
	"project": true, "projects": true, "decision": true, "decisions": true,
	"preference": true, "preferences": true, "reference": true, "feedback": true,
	"task": true, "context": true,
}

// sourceCategories is the kind of fact a source's events usually are, for a
// source that does not say. A meeting is time-bound, which is the only tier
// memory expires; a repository's events are about a project.
var sourceCategories = map[string]string{
	"github":  "project",
	"posthog": "project",
	"gcal":    "task",
}

// Category is the kind of fact this event is, for filing: Data["category"]
// when the source set a known one, else the usual kind for the source, else
// "context".
func (e Event) Category() string {
	if c, _ := e.Data["category"].(string); knownCategories[strings.ToLower(strings.TrimSpace(c))] {
		return strings.ToLower(strings.TrimSpace(c))
	}
	if c, ok := sourceCategories[e.Source]; ok {
		return c
	}
	return "context"
}

// Fact renders the event as one standalone statement worth keeping: what
// happened, where, and on which of the operator's accounts.
func (e Event) Fact() string {
	var b strings.Builder
	title := strings.TrimSpace(e.Title)
	summary := strings.TrimSpace(e.Summary)
	switch {
	case title != "" && summary != "":
		b.WriteString(strings.TrimRight(title, ".") + ". " + summary)
	case title != "":
		b.WriteString(title)
	default:
		b.WriteString(summary)
	}
	if b.Len() == 0 {
		return ""
	}
	b.WriteString("\n\n(" + e.Source + ", account " + e.Account)
	if e.Type != "" {
		b.WriteString(", " + e.Type)
	}
	if !e.At.IsZero() {
		b.WriteString(", " + e.At.UTC().Format(time.RFC3339))
	}
	if e.URL != "" {
		b.WriteString(", " + e.URL)
	}
	b.WriteString(")")
	return b.String()
}
