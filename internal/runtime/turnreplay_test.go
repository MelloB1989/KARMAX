package runtime

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func turnFor(t *testing.T, chatID string, startedAt time.Time) store.AgentTurn {
	t.Helper()
	evt := bus.NewEvent(bus.EventCommsMessage, "nexus", map[string]any{
		"channel_id": chatID, "text": "set up the daily reminder",
	})
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatal(err)
	}
	return store.AgentTurn{
		AgentID: "nexus", EventID: evt.ID, EventKind: string(bus.EventCommsMessage),
		EventJSON: string(raw), StartedAt: startedAt,
	}
}

func runtimeWithStore(t *testing.T) *KarmaxRuntime {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "t.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &KarmaxRuntime{store: db, log: zap.NewNop()}
}

// The replay that put four near-identical messages in one chat: the turn had
// already replied before the daemon died.
func TestATurnThatAlreadyRepliedIsNotResumed(t *testing.T) {
	rt := runtimeWithStore(t)
	started := time.Now().Add(-2 * time.Minute)
	turn := turnFor(t, "919999999999@s.whatsapp.net", started)

	if err := rt.store.SaveChannelMessage(store.StoredChannelMessage{
		ID: "m1", ChannelID: "919999999999@s.whatsapp.net", ChannelType: "whatsapp",
		Direction: string(comms.Outbound), Content: "on it — setting that up now",
		CreatedAt: started.Add(20 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if !rt.turnAlreadySpoke(turn) {
		t.Error("a turn that replied before the crash must not be replayed")
	}
}

// A turn interrupted before it said anything is exactly what resuming is for.
func TestASilentTurnIsStillResumed(t *testing.T) {
	rt := runtimeWithStore(t)
	turn := turnFor(t, "919999999999@s.whatsapp.net", time.Now().Add(-2*time.Minute))
	if rt.turnAlreadySpoke(turn) {
		t.Error("a turn that never replied must still be resumed — that is the whole point")
	}
}

// An earlier conversation in the same chat is not this turn's work.
func TestAnOlderMessageInTheSameChatDoesNotCount(t *testing.T) {
	rt := runtimeWithStore(t)
	started := time.Now()
	turn := turnFor(t, "919999999999@s.whatsapp.net", started)

	if err := rt.store.SaveChannelMessage(store.StoredChannelMessage{
		ID: "old", ChannelID: "919999999999@s.whatsapp.net", ChannelType: "whatsapp",
		Direction: string(comms.Outbound), Content: "yesterday's reply",
		CreatedAt: started.Add(-3 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if rt.turnAlreadySpoke(turn) {
		t.Error("a message sent before this turn began is not evidence this turn spoke")
	}
}

// Inbound messages are not KARMAX speaking.
func TestAnIncomingMessageIsNotEvidenceOfSpeaking(t *testing.T) {
	rt := runtimeWithStore(t)
	started := time.Now().Add(-time.Minute)
	turn := turnFor(t, "919999999999@s.whatsapp.net", started)

	if err := rt.store.SaveChannelMessage(store.StoredChannelMessage{
		ID: "in", ChannelID: "919999999999@s.whatsapp.net", ChannelType: "whatsapp",
		Direction: string(comms.Inbound), Content: "are you there?",
		CreatedAt: started.Add(10 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if rt.turnAlreadySpoke(turn) {
		t.Error("an inbound message must not be read as KARMAX having replied")
	}
}

// Timers and scheduled jobs have no chat, and nobody has heard them: they must
// stay resumable.
func TestNonChatTurnsStayResumable(t *testing.T) {
	rt := runtimeWithStore(t)
	turn := turnFor(t, "919999999999@s.whatsapp.net", time.Now())
	turn.EventKind = string(bus.EventScheduledJob)
	if rt.turnAlreadySpoke(turn) {
		t.Error("a scheduled job must remain resumable")
	}
}
