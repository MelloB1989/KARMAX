package sources

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
)

// The bus log stores a payload as JSON, so what a consumer reads back is not
// the Event a source published: Data is a generic map and the timestamp rides
// on the bus event. Decoding has to survive that round trip.
func TestAnEventSurvivesTheBusRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	in := Event{Source: "github", Account: "octo-work", Type: "issue_comment", Key: "n-7",
		Title: "New comment on acme/api#12", Summary: "Dana asked whether the fix ships today.",
		URL: "https://example.test/acme/api/12", At: at, Urgent: true,
		Data: map[string]any{"repo": "acme/api", "number": 12}}

	wire, err := json.Marshal(in.BusEvent("agent-1"))
	if err != nil {
		t.Fatal(err)
	}
	var evt bus.Event
	if err := json.Unmarshal(wire, &evt); err != nil {
		t.Fatal(err)
	}

	got, ok := FromBusEvent(evt)
	if !ok {
		t.Fatal("a published source event was not recognised after the bus round trip")
	}
	if got.Source != "github" || got.Account != "octo-work" || got.Type != "issue_comment" ||
		got.Key != "n-7" || got.Title != in.Title || got.Summary != in.Summary || got.URL != in.URL {
		t.Errorf("decoded %+v", got)
	}
	if !got.Urgent {
		t.Error("the urgent flag was lost")
	}
	if !got.At.Equal(at) {
		t.Errorf("At = %v, want %v", got.At, at)
	}
	if got.Data["repo"] != "acme/api" {
		t.Errorf("data = %v", got.Data)
	}
}

// A webhook from the HTTP server is also webhook.fired, and must not be taken
// for a source event: its payload has a route but no source and account.
func TestAPlainWebhookIsNotASourceEvent(t *testing.T) {
	evt := bus.NewEvent(bus.EventWebhookFired, "agent-1", map[string]any{
		"route": "/trigger", "method": "POST", "body": `{"x":1}`,
	})
	if _, ok := FromBusEvent(evt); ok {
		t.Error("a plain webhook was decoded as a source event")
	}
	if _, ok := FromBusEvent(bus.NewEvent(bus.EventCommsMessage, "a", map[string]any{"source": "x", "account": "y"})); ok {
		t.Error("only webhook.fired carries source events")
	}
}

// A fact filed from an event lands in the file of what it is ABOUT, not one
// file per source or one file for everything reflex ever filed.
func TestAnEventIsFiledUnderWhatItIsAbout(t *testing.T) {
	cases := []struct {
		name string
		e    Event
		want string
	}{
		{"an explicit subject wins", Event{Source: "github", Account: "octo", Data: map[string]any{"subject": "Billing revamp", "repo": "acme/api"}}, "Billing revamp"},
		{"a repo", Event{Source: "github", Account: "octo", Data: map[string]any{"repo": "acme/api"}}, "acme/api"},
		{"a person", Event{Source: "gchat", Account: "me@example.test", Data: map[string]any{"from": "Dana Reyes"}}, "Dana Reyes"},
		{"nothing to go on", Event{Source: "aws", Account: "prod"}, "aws prod"},
	}
	for _, c := range cases {
		if got := c.e.Subject(); got != c.want {
			t.Errorf("%s: Subject() = %q, want %q", c.name, got, c.want)
		}
	}
}

// What kind of fact an event is decides where it lives and whether it expires:
// a meeting is time-bound, a repository is a project.
func TestAnEventHasASensibleCategory(t *testing.T) {
	cases := []struct {
		e    Event
		want string
	}{
		{Event{Source: "github"}, "project"},
		{Event{Source: "gcal"}, "task"},
		{Event{Source: "slack"}, "context"},
		{Event{Source: "github", Data: map[string]any{"category": "decision"}}, "decision"},
		// A category the memory store does not know is ignored, not filed blind.
		{Event{Source: "github", Data: map[string]any{"category": "whatever"}}, "project"},
	}
	for _, c := range cases {
		if got := c.e.Category(); got != c.want {
			t.Errorf("%s %v: Category() = %q, want %q", c.e.Source, c.e.Data, got, c.want)
		}
	}
}
