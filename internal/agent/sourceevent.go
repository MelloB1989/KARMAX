package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/sources"
)

// Events from the operator's accounts.
//
// GitHub, Calendar, Chat, Slack, AWS, PostHog and the rest publish what
// happened on one account as a sources.Event, which arrives here as
// webhook.fired. The turn renders it as what a person would read — and,
// above all, WHICH account it happened on, because the agent acts through
// that same account.

// renderSourceEvent is the turn's task for an event from one of the operator's
// accounts.
func renderSourceEvent(e sources.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Something happened on one of the operator's accounts: **%s**, account **%s**.\n\n", e.Source, e.Account)
	if e.Urgent {
		b.WriteString("URGENT — the source marked this as something the operator must hear about now.\n\n")
	}
	if e.Type != "" {
		fmt.Fprintf(&b, "Type: %s\n", e.Type)
	}
	if e.Title != "" {
		fmt.Fprintf(&b, "What: %s\n", e.Title)
	}
	if e.Summary != "" {
		fmt.Fprintf(&b, "Detail: %s\n", truncateStr(e.Summary, 2000))
	}
	if e.URL != "" {
		fmt.Fprintf(&b, "Link: %s\n", e.URL)
	}
	if !e.At.IsZero() {
		fmt.Fprintf(&b, "When: %s (%s)\n", e.At.Local().Format("Mon 2 Jan 2006, 15:04 MST"), eventAge(e.At))
	}
	if details := renderEventData(e.Data); details != "" {
		b.WriteString("Details:\n" + details)
	}
	fmt.Fprintf(&b, "\nThis arrived on the operator's %s account %q. Anything you do about it — a reply, a comment, a lookup — goes through THAT account: the accounts section of your context says which tools act as which account. Never act through a different account unless the operator asked.\n", e.Source, e.Account)
	b.WriteString("- Decide what it needs and do it. Act on the operator's behalf as the operating policy in your context allows, including replying to people as them.\n" +
		"- File what is durable (who, what, when, a decision, a deadline) with memory.ingest, one fact per call; skip what is transient.\n" +
		"- If it needs nothing, stop there. Whether the operator hears about it is the operating policy's call, not a reflex.\n")
	return b.String()
}

// renderEventData lists a source's own fields, sorted so the same event always
// reads the same way.
func renderEventData(data map[string]any) string {
	if len(data) == 0 {
		return ""
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		var v string
		switch t := data[k].(type) {
		case nil:
			continue
		case string:
			v = strings.TrimSpace(t)
		case bool, float64, int, int64:
			v = fmt.Sprint(t)
		default:
			enc, err := json.Marshal(t)
			if err != nil {
				continue
			}
			v = string(enc)
		}
		if v == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", k, truncateStr(strings.ReplaceAll(v, "\n", " "), 300))
	}
	return b.String()
}

func eventAge(at time.Time) string {
	d := time.Since(at)
	switch {
	case d < 0:
		return "in " + (-d).Round(time.Minute).String()
	case d < time.Minute:
		return "just now"
	case d < 24*time.Hour:
		return d.Round(time.Minute).String() + " ago"
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// demandsAction reports whether the event is someone asking for something, so
// a turn that answers it without doing anything is a stall.
//
// An event from an account, or a raw webhook, asks nothing: deciding it needs
// no action is a correct outcome. Holding it to the act-evidence guard told the
// model "the operator gave you an instruction and you did nothing" about a
// notification nobody sent it.
func demandsAction(evt bus.Event) bool {
	return evt.Kind != bus.EventWebhookFired
}

// relatedMemoryLimit caps the facts an event brings with it.
const relatedMemoryLimit = 6

// buildRelatedMemoryContext is what long-term memory already holds about the
// subject of an account's event.
//
// A direct search, not the memory model: an LLM retrieval on every
// notification would spend a model turn per event before deciding whether the
// event needs one. The subject comes first because memory files one document
// per subject; the title catches what is filed under a different name.
func (a *Agent) buildRelatedMemoryContext(evt bus.Event) string {
	e, ok := sources.FromBusEvent(evt)
	if !ok || a.memory == nil {
		return ""
	}
	seen := map[string]bool{}
	var lines []string
	for _, q := range []string{e.Subject(), e.Title} {
		q = strings.TrimSpace(q)
		if q == "" || seen["q:"+q] {
			continue
		}
		seen["q:"+q] = true
		results, err := a.memory.Search(q, relatedMemoryLimit)
		if err != nil {
			continue
		}
		for _, r := range results {
			content := strings.TrimSpace(r.Entry.Content)
			if content == "" || seen[content] {
				continue
			}
			seen[content] = true
			line := "- " + truncateStr(strings.ReplaceAll(content, "\n", " "), 400)
			if !r.Entry.CreatedAt.IsZero() {
				line += " [stored " + r.Entry.CreatedAt.Format("2006-01-02") + "]"
			}
			if r.Entry.ID != "" {
				line += " (id: " + r.Entry.ID + ")"
			}
			lines = append(lines, line)
			if len(lines) >= relatedMemoryLimit {
				break
			}
		}
		if len(lines) >= relatedMemoryLimit {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "## What memory already holds about this\n\n" +
		"_Found by searching long-term memory for the event's subject. Old time-sensitive facts may be stale; correct one with memory.correct if this event changes it._\n\n" +
		strings.Join(lines, "\n") + "\n\n"
}
