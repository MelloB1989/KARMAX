package sources

import (
	"encoding/json"
	"testing"

	"github.com/MelloB1989/karmax/internal/bus"
)

// An event whose conversation a memory syncer reads says so across the bus,
// so a turn knows not to file it again.
func TestAMemorySyncedEventIsMarkedAcrossTheBus(t *testing.T) {
	e := Event{Source: "gchat", Account: "quinn@example.org", Title: "Dana: the launch moved", MemorySynced: true}
	wire, _ := json.Marshal(e.BusEvent("agent-1"))
	var evt bus.Event
	if err := json.Unmarshal(wire, &evt); err != nil {
		t.Fatal(err)
	}
	got, ok := FromBusEvent(evt)
	if !ok || !got.MemorySynced {
		t.Errorf("decoded %+v", got)
	}
	if plain, _ := FromBusEvent(Event{Source: "gchat", Account: "quinn@example.org"}.BusEvent("a")); plain.MemorySynced {
		t.Error("an ordinary event decoded as memory-synced")
	}
}

// The sources a memory syncer covers are named once, by the syncer that
// covers them; an event from one is marked on its way to a turn, and nothing
// else is.
func TestMemorySyncMarksOnlyTheSourcesItCovers(t *testing.T) {
	var m MemorySync
	m.Cover("gchat")
	covered := Event{Source: "gchat", Account: "quinn@example.org", Title: "a message"}.BusEvent("a")
	other := Event{Source: "github", Account: "octo", Title: "a review"}.BusEvent("a")

	got, _ := FromBusEvent(m.Mark(covered))
	if !got.MemorySynced {
		t.Error("an event from a covered source was not marked")
	}
	if _, set := covered.Payload["memory_synced"]; set {
		t.Error("marking changed the event as published, which other consumers share")
	}
	if got, _ := FromBusEvent(m.Mark(other)); got.MemorySynced {
		t.Error("an event from a source no syncer reads was marked")
	}
	plain := bus.NewEvent(bus.EventWebhookFired, "a", map[string]any{"route": "/trigger"})
	if out := m.Mark(plain); out.Payload["memory_synced"] != nil {
		t.Error("a plain webhook was marked")
	}

	var none *MemorySync
	if got, _ := FromBusEvent(none.Mark(covered)); got.MemorySynced {
		t.Error("no registry marked an event")
	}
	if !m.Covers("gchat") || m.Covers("github") || none.Covers("gchat") {
		t.Error("Covers disagrees with what was covered")
	}
}
