package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"time"

	"go.uber.org/zap"
)

// A restart used to be a hole in the record. wacli POSTs each message to
// KARMAX's webhook and retries for a few seconds; a daemon restart takes longer
// than that, so anything that arrived in the window was delivered to a closed
// port and was gone.
//
// wacli keeps every delivery with the exact body it tried to post. So a missed
// message is redelivered, not rebuilt: the channel finds the deliveries to its
// webhook that never landed and has wacli send each again. What arrives is the
// event the webhook would have carried — the chats the operator scoped it to,
// the group and mention facts, the sender's contact — through the same
// handler as everything else. A message that arrives twice keeps one id, so
// the store recognises the repeat (see messageKey).
//
// Rebuilding from wacli's message history is what this replaced, and it was
// wrong three ways. History rows carry no group flag, chat name or mentions, so
// a group message arrived as a direct chat and was answered as one, in the
// group. History holds every chat, so a restart delivered the ones the webhook
// is scoped away from: status updates, newsletters, groups nobody asked
// KARMAX to watch. And a message the webhook's own retry had delivered was
// routed a second time.
const (
	// WebhookPath is where wacli posts this channel's events.
	WebhookPath = "/comms/whatsapp"

	// maxReplayWindow bounds how far back redelivery reaches. A daemon that
	// was off for a week should come back to the conversation, not to a week
	// of stale messages it will answer as though they just arrived.
	maxReplayWindow = 12 * time.Hour
	// maxRedeliveries bounds one sweep, so a long outage cannot flood the
	// agent's mailbox faster than it drains; the next sweep takes the rest.
	maxRedeliveries = 200
	// The first sweep waits for the webhook to be listening, since a
	// redelivery to a closed port only fails again. Later sweeps catch
	// whatever failed while the daemon was up.
	firstSweepAfter = 20 * time.Second
	sweepEvery      = 5 * time.Minute
	wacliTimeout    = 60 * time.Second
)

// delivery is one of wacli's webhook delivery records.
type delivery struct {
	ID        int64     `json:"id"`
	WebhookID int64     `json:"webhook_id"`
	URL       string    `json:"url"`
	Event     string    `json:"event"`
	MessageID string    `json:"message_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// redeliverMissed sweeps for missed messages now and then until the channel
// stops.
func (w *WhatsAppChannel) redeliverMissed(ctx context.Context) {
	wait := firstSweepAfter
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		w.sweepMissed(ctx)
		wait = sweepEvery
	}
}

// sweepMissed has wacli redeliver every message event to this channel's
// webhook, in the window, that no delivery has landed.
func (w *WhatsAppChannel) sweepMissed(ctx context.Context) {
	failed, err := w.deliveries(ctx, "--status", "failed", "--limit", "1000")
	if err != nil {
		w.log.Warn("could not ask wacli which deliveries failed; missed messages wait for the next sweep",
			zap.String("channel_id", w.id), zap.Error(err))
		return
	}
	now := time.Now()
	tried := map[string]bool{}
	redelivered, failedAgain := 0, 0
	for _, d := range failed {
		// Messages only: a call that rang while the daemon was down is over.
		if d.Event != "incoming_message" && d.Event != "outgoing_message" {
			continue
		}
		if d.MessageID == "" || !postsHere(d.URL) || now.Sub(d.CreatedAt) > maxReplayWindow {
			continue
		}
		key := strconv.FormatInt(d.WebhookID, 10) + "/" + d.Event + "/" + d.MessageID
		if tried[key] {
			continue
		}
		tried[key] = true
		if redelivered+failedAgain >= maxRedeliveries {
			w.log.Warn("redelivery hit its cap for this sweep; the next sweep takes the rest",
				zap.String("channel_id", w.id), zap.Int("cap", maxRedeliveries))
			break
		}
		landed, err := w.landed(ctx, d)
		if err != nil || landed {
			continue
		}
		if err := w.redeliver(ctx, d.ID); err != nil {
			failedAgain++
			w.log.Warn("a missed message could not be redelivered; the next sweep tries again",
				zap.String("channel_id", w.id), zap.String("message_id", d.MessageID), zap.Error(err))
			continue
		}
		redelivered++
	}
	if redelivered > 0 || failedAgain > 0 {
		w.log.Info("redelivered messages the webhook missed",
			zap.String("channel_id", w.id), zap.Int("redelivered", redelivered), zap.Int("failed", failedAgain))
	}
}

// landed reports whether any delivery of d's event reached the webhook: a
// retry, or an earlier redelivery.
func (w *WhatsAppChannel) landed(ctx context.Context, d delivery) (bool, error) {
	all, err := w.deliveries(ctx, "--query", d.MessageID, "--limit", "100")
	if err != nil {
		return false, err
	}
	for _, o := range all {
		if o.WebhookID == d.WebhookID && o.Event == d.Event && o.MessageID == d.MessageID &&
			o.Status != "failed" {
			// Delivered, or wacli still retrying it: either way not ours to
			// send again.
			return true, nil
		}
	}
	return false, nil
}

func (w *WhatsAppChannel) deliveries(ctx context.Context, args ...string) ([]delivery, error) {
	out, err := w.wacli(ctx, append([]string{"webhooks", "deliveries"}, args...)...)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Deliveries []delivery `json:"webhook_deliveries"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("reading wacli's deliveries: %w", err)
	}
	return resp.Deliveries, nil
}

// redeliver has wacli post a recorded delivery again, body unchanged.
func (w *WhatsAppChannel) redeliver(ctx context.Context, id int64) error {
	out, err := w.wacli(ctx, "webhooks", "replay", "--id", strconv.FormatInt(id, 10))
	if err != nil {
		return err
	}
	var resp struct {
		OK       bool     `json:"ok"`
		Delivery delivery `json:"delivery"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return fmt.Errorf("reading wacli's redelivery: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("wacli's redelivery ended %q", resp.Delivery.Status)
	}
	return nil
}

func (w *WhatsAppChannel) wacli(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, wacliTimeout)
	defer cancel()
	return exec.CommandContext(ctx, w.wacliPath, args...).Output()
}

// postsHere reports whether a webhook URL is this channel's endpoint.
func postsHere(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Path == WebhookPath
}
