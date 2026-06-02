package discord

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/MelloB1989/karmax/internal/comms"
)

// DiscordChannel implements comms.Channel for Discord via discordgo.
type DiscordChannel struct {
	id      string
	token   string
	session *discordgo.Session
	inbox   chan comms.Message
	log     *zap.Logger
	ctx     context.Context
	cancel  context.CancelFunc
	botID   string
	mu      sync.RWMutex
}

// New creates a DiscordChannel with the given ID and bot token.
func New(id, token string, log *zap.Logger) *DiscordChannel {
	return &DiscordChannel{
		id:    id,
		token: token,
		inbox: make(chan comms.Message, 256),
		log:   log,
	}
}

func (d *DiscordChannel) ID() string   { return d.id }
func (d *DiscordChannel) Type() string { return "discord" }

// BotID returns the Discord user ID of the bot.
func (d *DiscordChannel) BotID() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.botID
}

// Start opens a Discord session, registers the message handler, and begins
// receiving messages.
func (d *DiscordChannel) Start(ctx context.Context) error {
	sess, err := discordgo.New("Bot " + d.token)
	if err != nil {
		return fmt.Errorf("create discord session: %w", err)
	}

	sess.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentMessageContent

	sess.AddHandler(d.handleMessage)
	sess.AddHandler(d.handleDisconnect)

	if err := sess.Open(); err != nil {
		return fmt.Errorf("open discord session: %w", err)
	}

	d.mu.Lock()
	d.session = sess
	d.botID = sess.State.User.ID
	d.ctx, d.cancel = context.WithCancel(ctx)
	d.mu.Unlock()

	d.log.Info("discord channel started",
		zap.String("channel_id", d.id),
		zap.String("bot_id", d.botID),
	)
	return nil
}

// handleDisconnect attempts to reconnect with exponential backoff when the
// Discord gateway connection is lost. It tries 3 times with delays of 2s, 4s,
// and 8s before giving up.
func (d *DiscordChannel) handleDisconnect(_ *discordgo.Session, _ *discordgo.Disconnect) {
	d.log.Warn("discord session disconnected, attempting reconnect",
		zap.String("channel_id", d.id),
	)

	backoff := 2 * time.Second
	const maxAttempts = 3

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		d.mu.RLock()
		ctx := d.ctx
		d.mu.RUnlock()

		if ctx != nil {
			select {
			case <-ctx.Done():
				d.log.Info("context cancelled, aborting reconnect",
					zap.String("channel_id", d.id),
				)
				return
			default:
			}
		}

		d.log.Info("reconnect attempt",
			zap.String("channel_id", d.id),
			zap.Int("attempt", attempt),
			zap.Duration("backoff", backoff),
		)

		time.Sleep(backoff)

		d.mu.RLock()
		sess := d.session
		d.mu.RUnlock()

		if sess == nil {
			d.log.Error("session is nil, cannot reconnect",
				zap.String("channel_id", d.id),
			)
			return
		}

		if err := sess.Open(); err != nil {
			d.log.Error("reconnect failed",
				zap.String("channel_id", d.id),
				zap.Int("attempt", attempt),
				zap.Error(err),
			)
			backoff *= 2
			continue
		}

		d.log.Info("reconnected successfully",
			zap.String("channel_id", d.id),
			zap.Int("attempt", attempt),
		)
		return
	}

	d.log.Error("all reconnect attempts exhausted",
		zap.String("channel_id", d.id),
	)
}

// handleMessage converts a discordgo MessageCreate into a comms.Message and
// sends it to the inbox. Messages from the bot itself are ignored.
func (d *DiscordChannel) handleMessage(_ *discordgo.Session, m *discordgo.MessageCreate) {
	d.mu.RLock()
	botID := d.botID
	d.mu.RUnlock()

	if m.Author == nil || m.Author.ID == botID {
		return
	}

	attachments := make([]comms.Attachment, 0, len(m.Attachments))
	for _, a := range m.Attachments {
		attachments = append(attachments, comms.Attachment{
			Filename: a.Filename,
			URL:      a.URL,
			MimeType: a.ContentType,
		})
	}

	replyToID := ""
	if m.ReferencedMessage != nil {
		replyToID = m.ReferencedMessage.ID
	}

	msg := comms.Message{
		ID:          uuid.New().String(),
		ChannelID:   m.ChannelID,
		ChannelType: "discord",
		SenderID:    m.Author.ID,
		SenderName:  m.Author.Username,
		Content:     m.Content,
		Direction:   comms.Inbound,
		ReplyToID:   replyToID,
		Attachments: attachments,
		Metadata: map[string]any{
			"guild_id":   m.GuildID,
			"message_id": m.ID,
		},
		Timestamp: time.Now(),
	}

	// Non-blocking send; drop the message if the inbox is full.
	select {
	case d.inbox <- msg:
	default:
		d.log.Warn("discord inbox full, dropping message",
			zap.String("channel_id", d.id),
			zap.String("discord_msg_id", m.ID),
		)
	}
}

// Stop closes the Discord session and cancels the context.
func (d *DiscordChannel) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cancel != nil {
		d.cancel()
	}
	if d.session != nil {
		return d.session.Close()
	}
	return nil
}

// Send sends a text message to the given Discord channel. Messages exceeding
// Discord's 2000-character limit are split into multiple sends.
func (d *DiscordChannel) Send(_ context.Context, channelID, content string) error {
	d.mu.RLock()
	sess := d.session
	d.mu.RUnlock()

	if sess == nil {
		return fmt.Errorf("discord session not started")
	}

	chunks := splitContent(content, 2000)
	for _, chunk := range chunks {
		if _, err := sess.ChannelMessageSend(channelID, chunk); err != nil {
			d.log.Error("failed to send discord message",
				zap.String("channel_id", d.id),
				zap.String("target", channelID),
				zap.Error(err),
			)
			return fmt.Errorf("send discord message: %w", err)
		}
	}
	return nil
}

// SendEmbed sends a rich embed to the given Discord channel.
func (d *DiscordChannel) SendEmbed(_ context.Context, channelID string, embed comms.Embed) error {
	d.mu.RLock()
	sess := d.session
	d.mu.RUnlock()

	if sess == nil {
		return fmt.Errorf("discord session not started")
	}

	fields := make([]*discordgo.MessageEmbedField, 0, len(embed.Fields))
	for _, f := range embed.Fields {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   f.Name,
			Value:  f.Value,
			Inline: f.Inline,
		})
	}

	discordEmbed := &discordgo.MessageEmbed{
		Title:       embed.Title,
		Description: embed.Description,
		Color:       embed.Color,
		Fields:      fields,
	}
	if embed.Footer != "" {
		discordEmbed.Footer = &discordgo.MessageEmbedFooter{
			Text: embed.Footer,
		}
	}

	if _, err := sess.ChannelMessageSendEmbed(channelID, discordEmbed); err != nil {
		return fmt.Errorf("send discord embed: %w", err)
	}
	return nil
}

// SendFile sends a file attachment to the given Discord channel.
func (d *DiscordChannel) SendFile(_ context.Context, channelID, filename string, data []byte) error {
	d.mu.RLock()
	sess := d.session
	d.mu.RUnlock()

	if sess == nil {
		return fmt.Errorf("discord session not started")
	}

	msg := &discordgo.MessageSend{
		Files: []*discordgo.File{
			{
				Name:   filename,
				Reader: bytes.NewReader(data),
			},
		},
	}

	if _, err := sess.ChannelMessageSendComplex(channelID, msg); err != nil {
		return fmt.Errorf("send discord file: %w", err)
	}
	return nil
}

// IncomingMessages returns the read-only channel of inbound messages.
func (d *DiscordChannel) IncomingMessages() <-chan comms.Message {
	return d.inbox
}

// splitContent breaks s into chunks of at most maxLen bytes.
func splitContent(s string, maxLen int) []string {
	if len(s) <= maxLen {
		return []string{s}
	}

	var chunks []string
	for len(s) > 0 {
		end := maxLen
		if end > len(s) {
			end = len(s)
		}
		chunks = append(chunks, s[:end])
		s = s[end:]
	}
	return chunks
}
