package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/sources"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func calendarEvent() bus.Event {
	return sources.Event{
		Source: "gcal", Account: "rowan@example.test", Type: "event.starting", Key: "ev-9",
		Title: "Design review with Contoso in 15 minutes", Summary: "Agenda: pricing page. Mateo is joining.",
		At: time.Now(), Urgent: true,
		Data: map[string]any{"calendar": "Work", "attendees": []any{"mateo@example.test"}},
	}.BusEvent("nexus")
}

// An event from an account is rendered as what happened and on which account,
// and says it is urgent when the source said so.
func TestASourceEventIsRenderedWithItsAccount(t *testing.T) {
	p := buildPromptFromEvent(calendarEvent(), nil)
	for _, want := range []string{"gcal", "rowan@example.test", "event.starting",
		"Design review with Contoso", "Mateo is joining", "calendar: Work", "URGENT"} {
		if !strings.Contains(p, want) {
			t.Errorf("the turn omits %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "```json") || strings.Contains(p, "raw system state") {
		t.Errorf("a source event was rendered as raw machine state:\n%s", p)
	}
}

// An LLM memory retrieval on every account event would spend a model turn per
// notification; the event's subject is searched directly instead.
func TestASourceEventDoesNotSpendAModelOnRetrieval(t *testing.T) {
	evt := calendarEvent()
	if shouldRetrieveMemory(evt, buildPromptFromEvent(evt, nil)) {
		t.Error("a source event triggered the LLM memory retrieval")
	}
}

// The act-evidence guard tells the model "the operator gave you an
// instruction and you did nothing". An account's event carries no instruction,
// and deciding it needs nothing is a correct outcome, not a stall.
func TestAnEventWithNoInstructionIsNotNudgedToAct(t *testing.T) {
	if demandsAction(calendarEvent()) {
		t.Error("an account event was treated as an instruction")
	}
	plain := bus.NewEvent(bus.EventWebhookFired, "nexus", map[string]any{"route": "/fleet", "body": "{}"})
	if demandsAction(plain) {
		t.Error("a raw webhook was treated as an instruction")
	}
	if !demandsAction(bus.NewEvent(bus.EventCommsMessage, "nexus", map[string]any{"content": "book the flight"})) {
		t.Error("an operator message must still be held to acting")
	}
}

// What memory already holds about the event's subject comes with the event,
// found by a direct search rather than a model.
func TestRelatedMemoryComesWithAnEvent(t *testing.T) {
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "t.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mem := memory.NewManager("nexus", "nexus", filepath.Join(dir, "mem"), db, zap.NewNop())
	if err := mem.Write(memory.MemoryEntry{Content: "The Work calendar is shared with the Contoso team.", Category: "context"}); err != nil {
		t.Fatal(err)
	}
	a := &Agent{memory: mem, log: zap.NewNop()}

	got := a.buildRelatedMemoryContext(calendarEvent())
	if !strings.Contains(got, "shared with the Contoso team") {
		t.Errorf("related memory missing:\n%s", got)
	}
	if a.buildRelatedMemoryContext(bus.NewEvent(bus.EventScheduledJob, "nexus", nil)) != "" {
		t.Error("an event with no subject searched memory")
	}
}

// A webhook's request headers can carry a caller's credentials and say nothing
// about what happened; they do not belong in a model's context.
func TestAWebhookTurnDoesNotCarryRequestHeaders(t *testing.T) {
	evt := bus.NewEvent(bus.EventWebhookFired, "nexus", map[string]any{
		"route": "/trigger", "method": "POST", "body": `{"deploy":"finished"}`,
		"headers": map[string]any{"Authorization": "Bearer not-a-real-token"},
	})
	p := buildPromptFromEvent(evt, nil)
	if strings.Contains(p, "not-a-real-token") || strings.Contains(p, "Authorization") {
		t.Errorf("request headers reached the turn:\n%s", p)
	}
	if !strings.Contains(p, "deploy") {
		t.Errorf("the body was lost:\n%s", p)
	}
}

// A message carries its related memory too, found by searching for what it
// says; a bare acknowledgement searches for nothing.
func TestRelatedMemoryComesWithAMessage(t *testing.T) {
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "t.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mem := memory.NewManager("nexus", "nexus", filepath.Join(dir, "mem"), db, zap.NewNop())
	if err := mem.Write(memory.MemoryEntry{Content: "The Contoso pilot starts on Monday with Mateo.", Category: "project"}); err != nil {
		t.Fatal(err)
	}
	a := &Agent{memory: mem, log: zap.NewNop()}

	msg := bus.NewEvent(bus.EventCommsMessage, "nexus", map[string]any{"content": "Contoso pilot", "channel_id": "x"})
	if got := a.buildRelatedMemoryContext(msg); !strings.Contains(got, "starts on Monday") {
		t.Errorf("related memory missing for a message:\n%s", got)
	}
	ack := bus.NewEvent(bus.EventCommsMessage, "nexus", map[string]any{"content": "ok thanks", "channel_id": "x"})
	if got := a.buildRelatedMemoryContext(ack); got != "" {
		t.Errorf("an acknowledgement searched memory:\n%s", got)
	}
}

// The recent-memory block is what was filed last, not what this is about, and
// must say so: read as context for the task, it misleads.
func TestRecentMemoryIsLabelledForWhatItIs(t *testing.T) {
	p := buildPromptFromEvent(bus.NewEvent(bus.EventScheduledJob, "nexus", map[string]any{"prompt": "check in"}),
		[]memory.MemoryEntry{{Role: "system", Content: "something filed earlier", CreatedAt: time.Now()}})
	if !strings.Contains(p, "not necessarily") {
		t.Errorf("the recent-memory block is not labelled:\n%s", p)
	}
}
