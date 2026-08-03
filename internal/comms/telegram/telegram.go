// Package telegram implements a KARMAX comms channel over the Telegram Bot API.
//
// It uses long-polling (getUpdates) rather than webhooks, so it works on a
// laptop, a Pi or anything behind NAT with no tunnel and no public URL — the
// same "runs on your own hardware" property as the rest of KARMAX.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	apiBase     = "https://api.telegram.org/bot"
	pollTimeout = 50 // seconds held open by Telegram per getUpdates call
	maxMessage  = 4096
)

// Channel is a Telegram bot exposed as a comms.Channel.
type Channel struct {
	id      string
	token   string
	inbox   chan comms.Message
	log     *zap.Logger
	http    *http.Client
	cancel  context.CancelFunc
	mu      sync.RWMutex
	offset  int64
	botUser string
	botID   int64
}

// New creates a Telegram channel. token comes from BotFather.
func New(id, token string, log *zap.Logger) *Channel {
	return &Channel{
		id:    id,
		token: strings.TrimSpace(token),
		inbox: make(chan comms.Message, 256),
		log:   log,
		// Slightly longer than the poll window so the request isn't cut short.
		http: &http.Client{Timeout: (pollTimeout + 15) * time.Second},
	}
}

func (c *Channel) ID() string   { return c.id }
func (c *Channel) Type() string { return "telegram" }

func (c *Channel) IncomingMessages() <-chan comms.Message { return c.inbox }

func (c *Channel) api(method string) string { return apiBase + c.token + "/" + method }

// call performs a Bot API call and unwraps Telegram's {ok, result} envelope.
func (c *Channel) call(ctx context.Context, method string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api(method), body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: bad response: %w", method, err)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if out != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// Start verifies the token and begins long-polling for updates.
func (c *Channel) Start(ctx context.Context) error {
	if c.token == "" {
		return fmt.Errorf("telegram: bot token is empty")
	}

	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := c.call(verifyCtx, "getMe", nil, &me)
	cancel()
	if err != nil {
		return fmt.Errorf("telegram: cannot authenticate: %w", err)
	}
	c.mu.Lock()
	c.botUser, c.botID = me.Username, me.ID
	c.mu.Unlock()

	runCtx, cancelRun := context.WithCancel(ctx)
	c.cancel = cancelRun
	go c.poll(runCtx)

	c.log.Info("telegram channel started",
		zap.String("channel_id", c.id), zap.String("bot", "@"+me.Username))
	return nil
}

func (c *Channel) Stop() error {
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64 `json:"message_id"`
		Date      int64 `json:"date"`
		Text      string
		Caption   string `json:"caption"`
		From      struct {
			ID        int64  `json:"id"`
			IsBot     bool   `json:"is_bot"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"from"`
		Chat struct {
			ID    int64  `json:"id"`
			Type  string `json:"type"` // private | group | supergroup | channel
			Title string `json:"title"`
		} `json:"chat"`
		ReplyToMessage *struct {
			MessageID int64  `json:"message_id"`
			Text      string `json:"text"`
			From      struct {
				ID    int64 `json:"id"`
				IsBot bool  `json:"is_bot"`
			} `json:"from"`
		} `json:"reply_to_message"`
		Entities []struct {
			Type   string `json:"type"`
			Offset int    `json:"offset"`
			Length int    `json:"length"`
		} `json:"entities"`
		Photo    []any `json:"photo"`
		Document *struct {
			FileName string `json:"file_name"`
			MimeType string `json:"mime_type"`
		} `json:"document"`
		Voice *any `json:"voice"`
	} `json:"message"`
}

// poll runs the long-poll loop until the context is cancelled.
func (c *Channel) poll(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		c.mu.RLock()
		offset := c.offset
		c.mu.RUnlock()

		var updates []tgUpdate
		err := c.call(ctx, "getUpdates", map[string]any{
			"offset":          offset,
			"timeout":         pollTimeout,
			"allowed_updates": []string{"message"},
		}, &updates)

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Network blips and Telegram hiccups are expected on a long poll —
			// back off rather than spinning, and never exit the loop.
			c.log.Warn("telegram poll failed", zap.String("channel_id", c.id), zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		for _, u := range updates {
			c.mu.Lock()
			if u.UpdateID >= c.offset {
				c.offset = u.UpdateID + 1 // ack: Telegram drops everything below
			}
			c.mu.Unlock()
			c.route(u)
		}
	}
}

// route converts a Telegram update into an inbound comms.Message.
func (c *Channel) route(u tgUpdate) {
	m := u.Message
	if m == nil || m.From.IsBot {
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
	// Telegram marks @mentions as entities; a reply to one of our messages is
	// equally "addressed to us". Both are computed here rather than left to the
	// model, matching how the WhatsApp channel behaves.
	mentionsMe := botUser != "" && strings.Contains(strings.ToLower(body), "@"+strings.ToLower(botUser))
	quotedIsFromMe := m.ReplyToMessage != nil && m.ReplyToMessage.From.ID == botID

	name := strings.TrimSpace(m.From.FirstName)
	if m.From.Username != "" {
		name = "@" + m.From.Username
	}
	if isGroup && m.Chat.Title != "" {
		name = m.Chat.Title
	}

	msg := comms.Message{
		ID:          uuid.New().String(),
		ChannelID:   strconv.FormatInt(m.Chat.ID, 10),
		ChannelType: "telegram",
		SenderID:    strconv.FormatInt(m.From.ID, 10),
		SenderName:  name,
		Content:     body,
		Direction:   comms.Inbound,
		Timestamp:   time.Unix(m.Date, 0),
		Metadata: map[string]any{
			"telegram_message_id": m.MessageID,
			"chat_id":             m.Chat.ID,
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
	for _, chunk := range chunks(content, maxMessage) {
		if err := c.call(ctx, "sendMessage", map[string]any{
			"chat_id":                  target,
			"text":                     chunk,
			"link_preview_options":     map[string]any{"is_disabled": true},
			"disable_web_page_preview": true,
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

// SendEmbed renders an embed as plain text — Telegram has no embed concept.
func (c *Channel) SendEmbed(ctx context.Context, target string, embed comms.Embed) error {
	var b strings.Builder
	if embed.Title != "" {
		b.WriteString(embed.Title + "\n\n")
	}
	if embed.Description != "" {
		b.WriteString(embed.Description)
	}
	for _, f := range embed.Fields {
		b.WriteString("\n\n" + f.Name + "\n" + f.Value)
	}
	return c.Send(ctx, target, b.String())
}

// SendFile uploads a document to the chat.
func (c *Channel) SendFile(ctx context.Context, target, filename string, data []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", target)
	part, err := w.CreateFormFile("document", filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api("sendDocument"), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram sendDocument: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// chunks splits s on line boundaries where possible, so long replies stay readable.
func chunks(s string, size int) []string {
	if len(s) <= size {
		return []string{s}
	}
	var out []string
	for len(s) > size {
		cut := strings.LastIndex(s[:size], "\n")
		if cut < size/2 {
			cut = size
		}
		out = append(out, s[:cut])
		s = strings.TrimLeft(s[cut:], "\n")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
