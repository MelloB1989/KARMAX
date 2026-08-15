package whatsapp

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"

	"go.uber.org/zap"
)

// A restart used to be a hole in the record. wacli POSTs each message to
// KARMAX's webhook and retries about five times over ~15 seconds; a daemon
// restart takes longer than that, so anything that arrived in the window was
// delivered to a closed port, exhausted its retries and was gone — no error
// anyone would see, because from wacli's side delivery merely failed, and from
// KARMAX's side the message never existed.
//
// The fix is to stop treating the webhook as the only path. wacli already
// stores every message, so on startup the channel asks what it missed and
// replays it through the ordinary route. The webhook stays the fast path; this
// is the one that makes it complete.
const (
	// maxReplayWindow bounds how far back a replay reaches. A daemon that was
	// off for a week should come back to the conversation, not to a week of
	// stale messages it will answer as though they just arrived.
	maxReplayWindow = 12 * time.Hour
	// maxReplayMessages bounds one catch-up, so a busy outage cannot flood the
	// agent's mailbox faster than it drains.
	maxReplayMessages = 200
	replayTimeout     = 30 * time.Second
)

// CursorStore remembers how far a channel has processed. An interface, so the
// comms package keeps knowing nothing about the database.
type CursorStore interface {
	CommsCursor(channelID string) (time.Time, bool, error)
	SetCommsCursor(channelID string, at time.Time, messageID string) error
}

// SetCursorStore enables restart-safe replay. Without one the channel behaves
// exactly as before: live webhooks only.
func (w *WhatsAppChannel) SetCursorStore(cs CursorStore) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cursors = cs
}

func (w *WhatsAppChannel) cursorStore() CursorStore {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.cursors
}

// noteProcessed advances the cursor past a message the channel has handled.
// Called for live traffic too — otherwise the cursor only ever moves during
// replay, and the next restart would replay everything since the last one.
func (w *WhatsAppChannel) noteProcessed(at time.Time, messageID string) {
	cs := w.cursorStore()
	if cs == nil || at.IsZero() {
		return
	}
	if err := cs.SetCommsCursor(w.id, at, messageID); err != nil {
		w.log.Warn("could not record how far this channel has processed",
			zap.String("channel_id", w.id), zap.Error(err))
	}
}

// replayMissed routes messages that arrived while the daemon was down.
func (w *WhatsAppChannel) replayMissed(ctx context.Context) {
	cs := w.cursorStore()
	if cs == nil {
		return
	}
	since, seen, err := cs.CommsCursor(w.id)
	if err != nil {
		w.log.Warn("could not read the replay cursor; skipping catch-up",
			zap.String("channel_id", w.id), zap.Error(err))
		return
	}
	if !seen {
		// First ever start: begin from now rather than replaying all history.
		w.noteProcessed(time.Now(), "")
		w.log.Info("replay cursor started from now", zap.String("channel_id", w.id))
		return
	}

	floor := time.Now().Add(-maxReplayWindow)
	clamped := false
	if since.Before(floor) {
		since, clamped = floor, true
	}

	msgs, err := w.messagesAfter(ctx, since)
	if err != nil {
		w.log.Warn("could not ask wacli what was missed",
			zap.String("channel_id", w.id), zap.Error(err))
		return
	}

	replayed, skipped := 0, 0
	for _, m := range msgs {
		if replayed >= maxReplayMessages {
			// Never silently. A capped replay that reports "done" is how a gap
			// becomes invisible again.
			w.log.Warn("replay hit its cap; older messages were left unprocessed",
				zap.String("channel_id", w.id), zap.Int("cap", maxReplayMessages),
				zap.Int("remaining", len(msgs)-replayed-skipped))
			break
		}
		// Our own sends are already accounted for, and a message at exactly the
		// cursor is the one we stopped on.
		if m.IsFromMe || !m.Timestamp.After(since) {
			skipped++
			continue
		}
		w.routeEvent(wacliWebhookEnvelope{
			Event: "incoming_message",
			Payload: wacliWebhookPayload{
				Source:  "whatsapp_event",
				Chat:    wacliChat{JID: m.ChatJID, Name: m.ChatName, IsGroup: m.IsGroup},
				Message: m.wacliMessage,
			},
		})
		replayed++
	}

	if replayed == 0 {
		w.log.Info("nothing was missed while the daemon was down",
			zap.String("channel_id", w.id), zap.Time("since", since))
		return
	}
	w.log.Info("replayed messages that arrived while the daemon was down",
		zap.String("channel_id", w.id), zap.Int("replayed", replayed),
		zap.Time("since", since), zap.Bool("window_clamped", clamped))
}

// replayMessage is one row of wacli's message history. It embeds the webhook's
// own message shape so a replayed message is byte-for-byte the same thing the
// live path handles — a replay that took a different shape would be a second
// code path to keep correct.
type replayMessage struct {
	wacliMessage
	ChatName string `json:"chat_name"`
	IsGroup  bool   `json:"is_group"`
}

func (w *WhatsAppChannel) messagesAfter(ctx context.Context, since time.Time) ([]replayMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, replayTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, w.wacliPath, "messages",
		"-after", since.Format(time.RFC3339),
		"-limit", "500",
	).Output()
	if err != nil {
		return nil, err
	}
	var resp struct {
		Messages []replayMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}
