package comms

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Sends that never passed through comms.
//
// comms records every message it sends, and every check that asks "was this
// already said" — the duplicate guard, a turn asking whether it already
// answered, a delivery cut off between sending and recording — reads that
// record. A message sent around comms was invisible to all of them: a
// harness shell's `wacli send`, a workflow's whatsapp_send_message, a task
// session that delivered its result itself. A channel that can see the
// account's own sends (wacli echoes each as an outgoing message) hands them
// here, and they are recorded like any other.

// ProducerWacli marks a send recorded from its echo: made through the
// channel's bridge directly rather than through comms. It may well be an
// agent's answer (a harness replying through its shell), so it counts as one.
const ProducerWacli = "wacli"

// Echo is a message the account sent that comms may not have sent itself.
type Echo struct {
	Target      string // the chat it went to
	Content     string // its text, or FileMessage(name) for a file
	ChannelType string
	MessageID   string // the bridge's own id for it
	At          time.Time
}

// EchoSender is a channel that reports the account's sends. The manager
// gives it the sink when it is registered.
type EchoSender interface {
	SetEchoSink(func(Echo))
}

// SentChecker is a channel that can say, from its own history, whether a
// message went out to a target since a moment — which holds the moment the
// send happens, before anything here records it.
type SentChecker interface {
	SentSince(ctx context.Context, target, content string, since time.Time) (bool, error)
}

// echoSettle is how long an echo waits before it is recorded: comms records
// its own send just after the channel returns, and the echo of that same send
// can arrive first.
var echoSettle = 3 * time.Second

// echoWindow is how far either side of an echo a recorded send with the same
// words counts as the same send.
const echoWindow = 2 * time.Minute

// recordEcho records a send seen only by its echo, unless comms already has.
func (m *Manager) recordEcho(e Echo) {
	content := strings.TrimSpace(e.Content)
	if m.store == nil || strings.TrimSpace(e.Target) == "" || content == "" {
		return
	}
	go func() {
		if echoSettle > 0 {
			time.Sleep(echoSettle)
		}
		at := e.At
		if at.IsZero() {
			at = time.Now()
		}
		if m.store.SentSince(e.Target, content, at.Add(-echoWindow)) {
			return // comms sent it, and recorded it
		}
		meta, _ := json.Marshal(map[string]any{"source": "echo", "message_id": e.MessageID})
		if err := m.store.SaveChannelMessage(store.StoredChannelMessage{
			ID: uuid.New().String(), ChannelID: e.Target, ChannelType: e.ChannelType,
			SenderID: ProducerWacli, Direction: string(Outbound), Content: content, Metadata: string(meta),
		}); err != nil {
			m.log.Warn("could not record a send seen by its echo", zap.String("target", e.Target), zap.Error(err))
		}
	}()
}

// SentSince reports whether content went out to target through channelID at
// or after since: in the record comms keeps, or in the channel's own history
// when it has one.
func (m *Manager) SentSince(channelID, target, content string, since time.Time) bool {
	if m.store != nil && m.store.SentSince(target, content, since) {
		return true
	}
	entry, ok := m.getEntry(channelID)
	if !ok {
		return false
	}
	checker, ok := entry.channel.(SentChecker)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sent, err := checker.SentSince(ctx, target, content, since)
	if err != nil {
		m.log.Warn("could not ask the channel what it sent", zap.String("channel_id", channelID), zap.Error(err))
		return false
	}
	return sent
}
