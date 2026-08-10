// Package telegram implements a KARMAX comms channel over the Bot API.
//
// Long polling, not webhooks: KARMAX runs on a laptop or a Pi behind somebody's
// router, so there is no public URL for Telegram to call. Polling means the
// connection is always outbound, like Slack's socket and wacli's local API.
//
// Built on gotgbot rather than hand-rolled HTTP. What was 390 lines of request
// building, JSON shapes and offset bookkeeping is now the parts that are
// actually KARMAX's: which updates are worth waking the agent for, and what the
// agent is told about them.
package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	// maxMessage is Telegram's per-message limit.
	maxMessage = 4096
	// pollTimeout is how long a getUpdates call waits for something to happen.
	// Long, because an idle poll costs nothing and a short one is a busy loop.
	pollTimeout = 50
)

// Channel is a Telegram bot exposed as a comms.Channel.
type Channel struct {
	id    string
	token string
	inbox chan comms.Message
	log   *zap.Logger

	bot    *gotgbot.Bot
	cancel context.CancelFunc

	mu      sync.RWMutex
	botID   int64
	botUser string
	offset  int64
}

// New creates a Telegram channel.
func New(id, token string, log *zap.Logger) *Channel {
	return &Channel{
		id:    id,
		token: strings.TrimSpace(token),
		inbox: make(chan comms.Message, 256),
		log:   log,
	}
}

func (c *Channel) ID() string                             { return c.id }
func (c *Channel) Type() string                           { return "telegram" }
func (c *Channel) IncomingMessages() <-chan comms.Message { return c.inbox }

// Start authenticates and begins polling.
func (c *Channel) Start(ctx context.Context) error {
	if c.token == "" {
		return fmt.Errorf("telegram: no bot token")
	}
	bot, err := gotgbot.NewBot(c.token, nil)
	if err != nil {
		return fmt.Errorf("telegram: the bot token was refused: %w", err)
	}
	c.bot = bot
	c.mu.Lock()
	c.botID, c.botUser = bot.User.Id, bot.User.Username
	c.mu.Unlock()
	c.log.Info("telegram connected",
		zap.String("bot", "@"+bot.User.Username), zap.String("channel", c.id))

	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	go c.poll(runCtx)
	return nil
}

// Stop ends polling.
func (c *Channel) Stop() error {
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

// poll pulls updates until the context ends.
func (c *Channel) poll(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		c.mu.RLock()
		offset := c.offset
		c.mu.RUnlock()

		updates, err := c.bot.GetUpdatesWithContext(ctx, &gotgbot.GetUpdatesOpts{
			Offset:  offset,
			Timeout: pollTimeout,
			// Only what we act on. Asking for everything means waking for edits,
			// reactions and poll answers that are then discarded.
			AllowedUpdates: []string{"message"},
			RequestOpts:    &gotgbot.RequestOpts{Timeout: (pollTimeout + 15) * time.Second},
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A failed poll is usually the network, and retrying immediately
			// would spin. The pause is short enough not to feel like downtime.
			c.log.Warn("telegram poll failed", zap.String("channel", c.id), zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		for _, u := range updates {
			// Advanced BEFORE handling: an update that makes routing panic would
			// otherwise be re-fetched forever, and the channel would never move
			// past it.
			c.mu.Lock()
			if u.UpdateId >= c.offset {
				c.offset = u.UpdateId + 1
			}
			c.mu.Unlock()
			c.route(u)
		}
	}
}

// route decides whether an update is worth waking the agent for, and what it
// should be told.
func (c *Channel) route(u gotgbot.Update) {
	m := u.Message
	if m == nil || m.From == nil || m.From.IsBot {
		return // ignore bots, including ourselves
	}

	body := strings.TrimSpace(m.Text)
	if body == "" {
		body = strings.TrimSpace(m.Caption)
	}
	// Media arrives with no text; leave a marker so the agent knows something
	// came in rather than silently dropping the event.
	switch {
	case len(m.Photo) > 0:
		body = strings.TrimSpace(body + " [received a photo]")
	case m.Document != nil:
		body = strings.TrimSpace(body + " [received a file: " + m.Document.FileName + "]")
	case m.Voice != nil:
		body = strings.TrimSpace(body + " [received a voice note]")
	}
	if body == "" {
		return
	}

	c.mu.RLock()
	botUser, botID := c.botUser, c.botID
	c.mu.RUnlock()

	isGroup := m.Chat.Type == "group" || m.Chat.Type == "supergroup"
	// Whether we were addressed is computed HERE rather than left to the model,
	// matching how the WhatsApp channel behaves: it changes whether KARMAX
	// speaks at all, which is too consequential to infer from prose.
	mentionsMe := botUser != "" && strings.Contains(strings.ToLower(body), "@"+strings.ToLower(botUser))
	quotedIsFromMe := m.ReplyToMessage != nil && m.ReplyToMessage.From != nil &&
		m.ReplyToMessage.From.Id == botID

	name := strings.TrimSpace(m.From.FirstName)
	if m.From.Username != "" {
		name = "@" + m.From.Username
	}
	if isGroup && m.Chat.Title != "" {
		name = m.Chat.Title
	}

	msg := comms.Message{
		ID:          uuid.New().String(),
		ChannelID:   strconv.FormatInt(m.Chat.Id, 10),
		ChannelType: "telegram",
		SenderID:    strconv.FormatInt(m.From.Id, 10),
		SenderName:  name,
		Content:     body,
		Direction:   comms.Inbound,
		Timestamp:   time.Unix(m.Date, 0),
		Metadata: map[string]any{
			"telegram_message_id": m.MessageId,
			"chat_id":             m.Chat.Id,
			"chat_type":           m.Chat.Type,
			"chat_name":           m.Chat.Title,
			"is_group":            isGroup,
			"mentions_me":         mentionsMe,
			"quoted_is_from_me":   quotedIsFromMe,
			"mention_count":       0,
		},
	}

	select {
	case c.inbox <- msg:
		c.log.Info("telegram message received",
			zap.String("channel_id", c.id),
			zap.String("chat", msg.ChannelID),
			zap.Bool("group", isGroup))
	default:
		c.log.Warn("telegram inbox full, dropping message", zap.String("channel_id", c.id))
	}
}

// Send delivers text to a chat id, splitting anything over Telegram's limit.
func (c *Channel) Send(ctx context.Context, target, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if c.bot == nil {
		return fmt.Errorf("telegram: not connected")
	}
	chatID, err := strconv.ParseInt(strings.TrimSpace(target), 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: %q is not a chat id", target)
	}
	for _, part := range Chunks(content, maxMessage) {
		if _, err := c.bot.SendMessageWithContext(ctx, chatID, part, &gotgbot.SendMessageOpts{
			RequestOpts: &gotgbot.RequestOpts{Timeout: 30 * time.Second},
		}); err != nil {
			return err
		}
	}
	return nil
}

// SendEmbed renders an embed as text, since Telegram has no embeds.
func (c *Channel) SendEmbed(ctx context.Context, target string, embed comms.Embed) error {
	var b strings.Builder
	if embed.Title != "" {
		b.WriteString(embed.Title + "\n\n")
	}
	if embed.Description != "" {
		b.WriteString(embed.Description + "\n")
	}
	for _, f := range embed.Fields {
		b.WriteString("\n" + f.Name + ": " + f.Value)
	}
	if embed.Footer != "" {
		b.WriteString("\n\n" + embed.Footer)
	}
	return c.Send(ctx, target, b.String())
}

// SendFile uploads a document.
func (c *Channel) SendFile(ctx context.Context, target, filename string, data []byte) error {
	if c.bot == nil {
		return fmt.Errorf("telegram: not connected")
	}
	chatID, err := strconv.ParseInt(strings.TrimSpace(target), 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: %q is not a chat id", target)
	}
	_, err = c.bot.SendDocumentWithContext(ctx, chatID,
		gotgbot.InputFileByReader(filename, strings.NewReader(string(data))),
		&gotgbot.SendDocumentOpts{RequestOpts: &gotgbot.RequestOpts{Timeout: 60 * time.Second}})
	return err
}

// Chunks splits a message on line boundaries so each part fits the limit.
//
// A message cut mid-word reads as a transmission error, so the break goes at a
// newline where there is one. A single line longer than the limit is cut,
// because there is nothing else to break on.
func Chunks(s string, limit int) []string {
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
