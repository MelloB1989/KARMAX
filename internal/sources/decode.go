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
	e.Backlog, _ = evt.Payload["backlog"].(bool)
	e.MemorySynced, _ = evt.Payload[memorySyncedKey].(bool)
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
//
// For each key a readable name wins over the id it stands for — "sender_name"
// over "sender", "channel_name" over "channel" — and a value that is only an
// id ("users/1047…", "spaces/AAAA…", "C0123ABCD45") is never a subject: memory
// would keep a document for an id nobody reads.
func (e Event) Subject() string {
	for _, k := range subjectKeys {
		for _, field := range []string{k + "_display_name", k + "_name", k} {
			if s, _ := e.Data[field].(string); strings.TrimSpace(s) != "" && !looksLikeID(s) {
				return strings.TrimSpace(s)
			}
		}
	}
	return strings.TrimSpace(e.Source + " " + e.Account)
}

// looksLikeID reports a value that identifies something without naming it: a
// number, a resource name whose last part is an id ("users/1047…",
// "spaces/AAAAMpdlehY", "people/c8812…"), or a service's opaque token
// ("C0123ABCD45"). Anything with a space in it is a name.
func looksLikeID(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || strings.ContainsAny(v, " \t\n") {
		return false
	}
	if allDigits(v) {
		return true
	}
	// A resource name: a lowercase collection ("users", "spaces") and an id.
	if parts := strings.Split(v, "/"); len(parts) >= 2 && isCollection(parts[0]) && opaqueToken(parts[len(parts)-1]) {
		return true
	}
	// A bare token of capitals and digits, as Slack and others mint them.
	return alnum(v) && len(v) >= 8 && countOf(v, isDigit) >= 2 && countOf(v, isLower) == 0
}

// opaqueToken is a path's last part when it is an id: digits, or letters and
// digits no one would write as a word (several digits, or several capitals).
func opaqueToken(s string) bool {
	if allDigits(s) {
		return true
	}
	return alnum(s) && len(s) >= 6 && (countOf(s, isDigit) >= 3 || countOf(s, isUpper) >= 3)
}

// isCollection is a resource collection's name: lowercase letters, plural.
func isCollection(s string) bool {
	return len(s) > 1 && strings.HasSuffix(s, "s") && countOf(s, isLower) == len(s)
}

func allDigits(s string) bool { return s != "" && countOf(s, isDigit) == len(s) }

func alnum(s string) bool {
	return countOf(s, func(r rune) bool { return isDigit(r) || isLower(r) || isUpper(r) }) == len(s)
}

func countOf(s string, f func(rune) bool) int {
	n := 0
	for _, r := range s {
		if f(r) {
			n++
		}
	}
	return n
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }
func isLower(r rune) bool { return r >= 'a' && r <= 'z' }
func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }

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
