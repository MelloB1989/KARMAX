// Package slack implements a KARMAX comms channel over Slack Socket Mode.
//
// Socket Mode is deliberate: Slack opens the connection *outbound* from this
// machine, so KARMAX needs no public URL, no tunnel and no inbound firewall
// rule — the same self-hosted property as the Telegram and WhatsApp channels.
//
// Two tokens are required:
//   - an app-level token (xapp-…) with connections:write — opens the socket
//   - a bot token (xoxb-…) — reads and posts messages
//
// Built on slack-go rather than hand-rolled HTTP. The previous version was 500
// lines reimplementing the socket handshake, the envelope acks, the reconnect
// backoff and the JSON shapes — all of which Slack changes and none of which is
// KARMAX's problem to track.
package slack

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"
)

// maxMessage leaves room under Slack's 4000-character limit for formatting.
const maxMessage = 3800

// Channel is a Slack bot exposed as a comms.Channel.
type Channel struct {
	id       string
	appToken string // xapp-… : opens the Socket Mode connection
	botToken string // xoxb-… : reads and posts
	inbox    chan comms.Message
	// decisions carries Approve/Reject outcomes to the comms manager — the
	// same shape IncomingMessages already uses to surface inbound things, so
	// the manager drains this exactly like it drains a message channel.
	decisions chan comms.Decision
	log       *zap.Logger
	cancel    context.CancelFunc

	api    *slack.Client
	socket *socketmode.Client

	mu            sync.RWMutex
	botID         string // our bot user id, for self/mention detection
	signingSecret string // verifies the HTTP paths; Socket Mode doesn't need it
	// Slack events carry user IDs, not names. Resolved names are cached so a
	// busy channel does not cost one users.info call per message.
	names map[string]string

	// dedup remembers envelope/event ids already processed, so a redelivery
	// (Slack resends anything not acked within 3s, and recycles connections
	// routinely) does not double-run a step that isn't safe to repeat.
	dedup *envelopeDedup
}

// New creates a Slack channel. appToken opens the socket, botToken does the work.
func New(id, appToken, botToken string, log *zap.Logger) *Channel {
	return &Channel{
		id:        id,
		appToken:  strings.TrimSpace(appToken),
		botToken:  strings.TrimSpace(botToken),
		inbox:     make(chan comms.Message, 256),
		decisions: make(chan comms.Decision, 64),
		log:       log,
		names:     make(map[string]string),
		dedup:     newEnvelopeDedup(),
	}
}

// SetSigningSecret enables request verification on the HTTP paths (Events API
// and Interactivity webhooks, for installs with ingress). Socket Mode doesn't
// need it — the app token already authenticates that connection — so a
// socket-only install can leave this unset.
func (c *Channel) SetSigningSecret(secret string) {
	c.mu.Lock()
	c.signingSecret = strings.TrimSpace(secret)
	c.mu.Unlock()
}

func (c *Channel) secret() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.signingSecret
}

// Decisions implements comms.DecisionSource: an Approve/Reject click, once
// verified and resolved, lands here for the manager to publish.
func (c *Channel) Decisions() <-chan comms.Decision { return c.decisions }

func (c *Channel) ID() string                             { return c.id }
func (c *Channel) Type() string                           { return "slack" }
func (c *Channel) IncomingMessages() <-chan comms.Message { return c.inbox }

// Start opens the socket and pumps events until the context ends.
func (c *Channel) Start(ctx context.Context) error {
	if c.appToken == "" || c.botToken == "" {
		return fmt.Errorf("slack: both an app token (xapp-…) and a bot token (xoxb-…) are required")
	}
	if !strings.HasPrefix(c.appToken, "xapp-") {
		return fmt.Errorf("slack: the app token should start with xapp- — %q looks like the wrong one", short(c.appToken))
	}

	c.api = slack.New(c.botToken, slack.OptionAppLevelToken(c.appToken))
	auth, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("slack: the bot token was refused: %w", err)
	}
	c.mu.Lock()
	c.botID = auth.UserID
	c.mu.Unlock()
	c.log.Info("slack connected",
		zap.String("team", auth.Team), zap.String("bot", auth.User), zap.String("channel", c.id))

	c.socket = socketmode.New(c.api)
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	go c.pump(runCtx)
	go c.runSocket(runCtx)
	return nil
}

// reconnectMinBackoff/reconnectMaxBackoff bound the wait between attempts to
// restart the socket after RunContext gives up entirely. Slack's own client
// already retries ordinary drops internally (pings, "please reconnect"
// disconnects) — RunContext only returns when that internal loop concluded
// the connection is unrecoverable (or our context was cancelled). Treating
// that as final was the bug: an auth hiccup or a Slack-side blip then meant
// this channel never spoke again until the whole process restarted.
const (
	reconnectMinBackoff = 2 * time.Second
	reconnectMaxBackoff = 5 * time.Minute
	// reconnectResetAfter: a connection that stayed up this long before
	// failing is a fresh problem, not a continuation of the last one, so the
	// wait resets instead of climbing forever over the channel's lifetime.
	reconnectResetAfter = 5 * time.Minute
)

// runSocket keeps the socket connection alive for as long as ctx lives,
// re-dialing with backoff whenever RunContext exits early.
func (c *Channel) runSocket(ctx context.Context) {
	backoff := reconnectMinBackoff
	for {
		started := time.Now()
		err := c.socket.RunContext(ctx)
		if ctx.Err() != nil {
			return // shutting down, not a failure
		}
		if time.Since(started) > reconnectResetAfter {
			backoff = reconnectMinBackoff
		}
		c.log.Error("slack socket stopped; reconnecting",
			zap.String("channel", c.id), zap.Error(err), zap.Duration("wait", backoff))

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < reconnectMaxBackoff {
			backoff *= 2
			if backoff > reconnectMaxBackoff {
				backoff = reconnectMaxBackoff
			}
		}
	}
}

// pump turns socket events into KARMAX messages.
func (c *Channel) pump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-c.socket.Events:
			if !ok {
				return
			}
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				api, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				// Acked immediately, before any work: Slack redelivers anything
				// unacknowledged within three seconds, and a slow turn would
				// otherwise become the same message handled twice.
				if evt.Request != nil {
					c.socket.Ack(*evt.Request)
				}
				// Even acked in time, a redelivery still happens (a reconnect
				// mid-flight, a retried envelope) — the envelope id catches what
				// the ack window alone cannot.
				if c.envelopeSeen(evt.Request) {
					continue
				}
				c.handleEvent(ctx, api)
			case socketmode.EventTypeInteractive:
				cb, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					continue
				}
				if evt.Request != nil {
					c.socket.Ack(*evt.Request)
				}
				if c.envelopeSeen(evt.Request) {
					continue
				}
				c.handleInteraction(ctx, cb)
			case socketmode.EventTypeConnectionError:
				c.log.Warn("slack connection error; the client will retry", zap.Any("data", evt.Data))
			case socketmode.EventTypeInvalidAuth:
				c.log.Error("slack rejected the tokens — reconnecting will not help",
					zap.String("channel", c.id))
			}
		}
	}
}

// envelopeSeen reports whether this socket envelope has already been
// processed. req is nil for event types that don't carry one (hello,
// connection lifecycle events), which never need deduping.
func (c *Channel) envelopeSeen(req *socketmode.Request) bool {
	if req == nil || req.EnvelopeID == "" {
		return false
	}
	return c.dedup.seenBefore(req.EnvelopeID, time.Now())
}

// handleEvent forwards a user message and ignores everything else.
func (c *Channel) handleEvent(ctx context.Context, api slackevents.EventsAPIEvent) {
	if api.Type != slackevents.CallbackEvent {
		return
	}
	inner, ok := api.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		// Also accept an app mention, which is how a bot is addressed in a
		// channel it is not a member of.
		if mention, ok := api.InnerEvent.Data.(*slackevents.AppMentionEvent); ok {
			c.deliver(ctx, mention.User, mention.Channel, mention.Text, mention.TimeStamp, mention.ThreadTimeStamp)
		}
		return
	}
	// A bot's own messages come back on the socket; forwarding them would have
	// KARMAX answering itself.
	c.mu.RLock()
	self := c.botID
	c.mu.RUnlock()
	if inner.BotID != "" || inner.User == "" || inner.User == self {
		return
	}
	// Edits, deletions and joins arrive as messages with a subtype.
	if inner.SubType != "" {
		return
	}
	c.deliver(ctx, inner.User, inner.Channel, inner.Text, inner.TimeStamp, inner.ThreadTimeStamp)
}

func (c *Channel) deliver(ctx context.Context, user, channel, text, ts, threadTS string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	msg := comms.Message{
		ID: ts,
		// The Slack conversation, not this transport's name. ChannelID is what a
		// reply is addressed to and what the manager remembers as the last known
		// target — recording the transport here meant a reply was sent to the
		// string "slack-main", and the same confusion reached WhatsApp through
		// the event payload. Every other channel puts the conversation here.
		ChannelID:   channel,
		ChannelType: "slack",
		SenderID:    user,
		SenderName:  c.userName(ctx, user),
		Content:     text,
		Direction:   comms.Inbound,
		ReplyToID:   threadTS,
		Timestamp:   time.Now(),
		Metadata:    map[string]any{"slack_channel": channel, "thread_ts": threadTS},
	}
	select {
	case c.inbox <- msg:
	default:
		// Dropped rather than blocking the socket: a full inbox means the agent
		// is behind, and stalling the event pump would stop the acks too.
		c.log.Warn("slack inbox is full; dropped a message", zap.String("channel", channel))
	}
}

// userName resolves a Slack user ID to a display name, caching the result.
func (c *Channel) userName(ctx context.Context, userID string) string {
	if userID == "" {
		return ""
	}
	c.mu.RLock()
	name, ok := c.names[userID]
	c.mu.RUnlock()
	if ok {
		return name
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	user, err := c.api.GetUserInfoContext(lookupCtx, userID)
	if err != nil {
		// The id is a worse name than a name, and a better one than nothing.
		c.log.Debug("slack users.info failed", zap.String("user", userID), zap.Error(err))
		return userID
	}
	name = firstNonEmpty(user.Profile.DisplayName, user.Profile.RealName, user.Name, userID)

	c.mu.Lock()
	c.names[userID] = name
	c.mu.Unlock()
	return name
}

// Send posts a message, threading it when the target names a thread.
//
// The target is "<channel>" or "<channel>:<thread_ts>", so a reply lands under
// the message it answers rather than at the bottom of the channel.
func (c *Channel) Send(ctx context.Context, target, content string) error {
	if c.api == nil {
		return fmt.Errorf("slack: not connected")
	}
	channel, thread := splitTarget(target)
	if channel == "" {
		return fmt.Errorf("slack: no channel to send to")
	}

	// Split rather than truncated. A long answer cut at 3800 characters is an
	// answer that stops mid-sentence, and the operator has no way to ask for
	// the rest — so it goes as several messages, broken on line boundaries.
	for _, part := range chunks(content, maxMessage) {
		opts := []slack.MsgOption{
			slack.MsgOptionText(part, false),
			slack.MsgOptionDisableLinkUnfurl(),
		}
		if thread != "" {
			opts = append(opts, slack.MsgOptionTS(thread))
		}
		if _, _, err := c.api.PostMessageContext(ctx, channel, opts...); err != nil {
			return err
		}
	}
	return nil
}

// SendEmbed renders an embed as a Slack attachment.
func (c *Channel) SendEmbed(ctx context.Context, target string, embed comms.Embed) error {
	if c.api == nil {
		return fmt.Errorf("slack: not connected")
	}
	channel, thread := splitTarget(target)
	att := slack.Attachment{
		Title:      embed.Title,
		Text:       truncate(embed.Description, maxMessage),
		Footer:     embed.Footer,
		MarkdownIn: []string{"text", "fields"},
	}
	if embed.Color != 0 {
		att.Color = fmt.Sprintf("#%06x", embed.Color&0xFFFFFF)
	}
	for _, f := range embed.Fields {
		att.Fields = append(att.Fields, slack.AttachmentField{
			Title: f.Name, Value: f.Value, Short: f.Inline,
		})
	}
	opts := []slack.MsgOption{slack.MsgOptionAttachments(att)}
	if thread != "" {
		opts = append(opts, slack.MsgOptionTS(thread))
	}
	_, _, err := c.api.PostMessageContext(ctx, channel, opts...)
	return err
}

// SendFile uploads a file to a channel.
func (c *Channel) SendFile(ctx context.Context, target, filename string, data []byte) error {
	if c.api == nil {
		return fmt.Errorf("slack: not connected")
	}
	channel, thread := splitTarget(target)
	_, err := c.api.UploadFileContext(ctx, slack.UploadFileParameters{
		Channel:         channel,
		Filename:        filename,
		FileSize:        len(data),
		Reader:          bytes.NewReader(data),
		ThreadTimestamp: thread,
	})
	return err
}

// Stop closes the socket.
func (c *Channel) Stop() error {
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

// splitTarget separates "<channel>:<thread_ts>".
//
// A Slack timestamp contains a dot and no colon, so this cannot be confused
// with one — which is why the separator is a colon rather than the more obvious
// slash a channel name may contain.
func splitTarget(target string) (channel, thread string) {
	target = strings.TrimSpace(target)
	if i := strings.LastIndexByte(target, ':'); i > 0 {
		return target[:i], target[i+1:]
	}
	return target, ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// chunks splits a message on line boundaries so each part fits.
//
// Line boundaries rather than a hard cut, because a message split mid-word
// reads as a transmission error, and code or a list split mid-line is worse.
// A single line longer than the limit is cut, since there is nothing else to
// break on.
func chunks(s string, limit int) []string {
	if len(s) <= limit {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(s, "\n") {
		for len(line) > limit {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			out = append(out, line[:limit])
			line = line[limit:]
		}
		// +1 for the newline this line would add.
		if cur.Len()+len(line)+1 > limit && cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}
