// Package slack implements a KARMAX comms channel over Slack Socket Mode.
//
// Socket Mode is deliberate: Slack opens the connection *outbound* from this
// machine, so KARMAX needs no public URL, no tunnel and no inbound firewall
// rule — the same self-hosted property as the Telegram and WhatsApp channels.
//
// Two tokens are required:
//   - an app-level token (xapp-…) with connections:write — opens the socket
//   - a bot token (xoxb-…) — reads and posts messages
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

const (
	apiBase    = "https://slack.com/api/"
	maxMessage = 3800 // Slack's hard limit is 4000; leave room for formatting
)

// Channel is a Slack bot exposed as a comms.Channel.
type Channel struct {
	id       string
	appToken string // xapp-… : opens the Socket Mode connection
	botToken string // xoxb-… : reads and posts
	inbox    chan comms.Message
	log      *zap.Logger
	http     *http.Client
	cancel   context.CancelFunc

	mu     sync.RWMutex
	botID  string // our bot user id, for self/mention detection
	teamID string
	// Slack message events carry user IDs, not names. Resolved names are cached
	// so a busy channel doesn't cost one users.info call per message.
	names map[string]string
}

// New creates a Slack channel. appToken opens the socket, botToken does the work.
func New(id, appToken, botToken string, log *zap.Logger) *Channel {
	return &Channel{
		id:       id,
		appToken: strings.TrimSpace(appToken),
		botToken: strings.TrimSpace(botToken),
		inbox:    make(chan comms.Message, 256),
		log:      log,
		http:     &http.Client{Timeout: 30 * time.Second},
		names:    make(map[string]string),
	}
}

// userName resolves a Slack user ID to a display name, caching the result.
// Failures fall back to the raw ID rather than blocking the message.
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

	var resp struct {
		User struct {
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.get(lookupCtx, "users.info?user="+url.QueryEscape(userID), &resp); err != nil {
		c.log.Debug("slack users.info failed", zap.String("user", userID), zap.Error(err))
		return userID
	}

	name = resp.User.Profile.DisplayName
	if name == "" {
		name = resp.User.Profile.RealName
	}
	if name == "" {
		name = resp.User.Name
	}
	if name == "" {
		name = userID
	}
	c.mu.Lock()
	c.names[userID] = name
	c.mu.Unlock()
	return name
}

func (c *Channel) ID() string                             { return c.id }
func (c *Channel) Type() string                           { return "slack" }
func (c *Channel) IncomingMessages() <-chan comms.Message { return c.inbox }

// call performs a Slack Web API request and unwraps the {ok, error} envelope.
func (c *Channel) call(ctx context.Context, method, token string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+method, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("slack %s: bad response: %w", method, err)
	}
	if !env.OK {
		return fmt.Errorf("slack %s: %s", method, env.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// get performs a read-only Slack Web API call whose arguments are in the query
// string. Separate from call() because the write methods must stay POST+JSON.
func (c *Channel) get(ctx context.Context, method string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+method, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.botToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("slack %s: bad response: %w", method, err)
	}
	if !env.OK {
		return fmt.Errorf("slack %s: %s", method, env.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Start authenticates and runs the Socket Mode loop.
func (c *Channel) Start(ctx context.Context) error {
	if c.appToken == "" || c.botToken == "" {
		return fmt.Errorf("slack: both app token (xapp-) and bot token (xoxb-) are required")
	}

	var auth struct {
		UserID string `json:"user_id"`
		TeamID string `json:"team_id"`
		Team   string `json:"team"`
	}
	authCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := c.call(authCtx, "auth.test", c.botToken, nil, &auth)
	cancel()
	if err != nil {
		return fmt.Errorf("slack: cannot authenticate: %w", err)
	}
	c.mu.Lock()
	c.botID, c.teamID = auth.UserID, auth.TeamID
	c.mu.Unlock()

	runCtx, cancelRun := context.WithCancel(ctx)
	c.cancel = cancelRun
	go c.run(runCtx)

	c.log.Info("slack channel started",
		zap.String("channel_id", c.id), zap.String("team", auth.Team), zap.String("bot", auth.UserID))
	return nil
}

func (c *Channel) Stop() error {
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

// run keeps a Socket Mode connection alive, reconnecting with backoff. Slack
// recycles these sockets routinely, so reconnecting is normal operation rather
// than an error path.
func (c *Channel) run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.connectOnce(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("slack socket closed", zap.String("channel_id", c.id), zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

type socketEnvelope struct {
	Type        string          `json:"type"`
	EnvelopeID  string          `json:"envelope_id"`
	Payload     json.RawMessage `json:"payload"`
	AcceptsResp bool            `json:"accepts_response_payload"`
	Reason      string          `json:"reason"`
}

type eventPayload struct {
	Event struct {
		Type        string `json:"type"`
		Subtype     string `json:"subtype"`
		Text        string `json:"text"`
		User        string `json:"user"`
		BotID       string `json:"bot_id"`
		Channel     string `json:"channel"`
		ChannelType string `json:"channel_type"` // im | channel | group | mpim
		TS          string `json:"ts"`
		ThreadTS    string `json:"thread_ts"`
		Files       []struct {
			Name     string `json:"name"`
			Mimetype string `json:"mimetype"`
		} `json:"files"`
	} `json:"event"`
}

// connectOnce opens one Socket Mode connection and pumps it until it dies.
func (c *Channel) connectOnce(ctx context.Context) error {
	var conn struct {
		URL string `json:"url"`
	}
	openCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err := c.call(openCtx, "apps.connections.open", c.appToken, nil, &conn)
	cancel()
	if err != nil {
		return fmt.Errorf("apps.connections.open: %w", err)
	}

	ws, _, err := websocket.DefaultDialer.DialContext(ctx, conn.URL, nil)
	if err != nil {
		return fmt.Errorf("dial socket: %w", err)
	}
	defer ws.Close()
	c.log.Info("slack socket connected", zap.String("channel_id", c.id))

	go func() { // close the socket when the runtime shuts down
		<-ctx.Done()
		_ = ws.Close()
	}()

	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		var env socketEnvelope
		if json.Unmarshal(raw, &env) != nil {
			continue
		}

		switch env.Type {
		case "hello":
			continue
		case "disconnect":
			// Slack asks us to reconnect (refresh / server rotation).
			return fmt.Errorf("slack asked to disconnect: %s", env.Reason)
		}

		// Every envelope must be acked or Slack redelivers it.
		if env.EnvelopeID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			if err := ws.WriteMessage(websocket.TextMessage, ack); err != nil {
				return fmt.Errorf("ack: %w", err)
			}
		}

		if env.Type == "events_api" && len(env.Payload) > 0 {
			var p eventPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				c.route(p)
			}
		}
	}
}

var slackMention = regexp.MustCompile(`<@([A-Z0-9]+)>`)

// route converts a Slack message event into an inbound comms.Message.
func (c *Channel) route(p eventPayload) {
	e := p.Event
	if e.Type != "message" || e.BotID != "" {
		return // not a human message (or it's us)
	}
	// Edits, deletions, joins and similar carry a subtype we don't act on.
	if e.Subtype != "" && e.Subtype != "file_share" {
		return
	}

	c.mu.RLock()
	botID := c.botID
	c.mu.RUnlock()
	if e.User == botID || e.User == "" {
		return
	}

	body := strings.TrimSpace(e.Text)
	for _, f := range e.Files {
		body = strings.TrimSpace(body + " [received a file: " + f.Name + "]")
	}
	if body == "" {
		return
	}

	isDM := e.ChannelType == "im"
	// Deterministic addressing signals, mirroring the WhatsApp/Telegram
	// channels: a <@BOT> mention, and how many people were tagged (so an
	// @here-style blast can be told apart from being addressed directly).
	mentions := slackMention.FindAllStringSubmatch(body, -1)
	mentionsMe := false
	for _, m := range mentions {
		if len(m) > 1 && m[1] == botID {
			mentionsMe = true
			break
		}
	}
	// Render <@U123> as @display-name. Raw Slack IDs in the text are unreadable
	// for both the operator and the model; the name cache makes this cheap.
	lookupCtx := context.Background()
	body = slackMention.ReplaceAllStringFunc(body, func(tag string) string {
		m := slackMention.FindStringSubmatch(tag)
		if len(m) < 2 {
			return tag
		}
		return "@" + c.userName(lookupCtx, m[1])
	})

	msg := comms.Message{
		ID:          uuid.New().String(),
		ChannelID:   e.Channel,
		ChannelType: "slack",
		SenderID:    e.User,
		SenderName:  c.userName(lookupCtx, e.User),
		Content:     body,
		Direction:   comms.Inbound,
		Timestamp:   time.Now(),
		Metadata: map[string]any{
			"slack_ts":          e.TS,
			"thread_ts":         e.ThreadTS,
			"channel_type":      e.ChannelType,
			"is_group":          !isDM,
			"mentions_me":       mentionsMe,
			"quoted_is_from_me": false,
			"mention_count":     len(mentions),
		},
	}

	select {
	case c.inbox <- msg:
		c.log.Info("slack message received",
			zap.String("channel_id", c.id), zap.String("chat", e.Channel), zap.Bool("dm", isDM))
	default:
		c.log.Warn("slack inbox full, dropping message", zap.String("channel_id", c.id))
	}
}

// Send posts text to a channel or DM. target is a Slack channel ID.
func (c *Channel) Send(ctx context.Context, target, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	for _, chunk := range chunks(content, maxMessage) {
		if err := c.call(ctx, "chat.postMessage", c.botToken, map[string]any{
			"channel": target,
			"text":    chunk,
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

// SendEmbed renders an embed as Slack mrkdwn.
func (c *Channel) SendEmbed(ctx context.Context, target string, embed comms.Embed) error {
	var b strings.Builder
	if embed.Title != "" {
		b.WriteString("*" + embed.Title + "*\n")
	}
	if embed.Description != "" {
		b.WriteString(embed.Description)
	}
	for _, f := range embed.Fields {
		b.WriteString("\n\n*" + f.Name + "*\n" + f.Value)
	}
	return c.Send(ctx, target, b.String())
}

// SendFile uploads a file using the external-upload flow (files.upload is
// retired). Falls back to posting a notice if the upload can't be completed.
func (c *Channel) SendFile(ctx context.Context, target, filename string, data []byte) error {
	var up struct {
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := c.call(ctx, "files.getUploadURLExternal", c.botToken, map[string]any{
		"filename": filename,
		"length":   len(data),
	}, &up); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.UploadURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("slack upload: %s", resp.Status)
	}

	files, _ := json.Marshal([]map[string]string{{"id": up.FileID, "title": filename}})
	return c.call(ctx, "files.completeUploadExternal", c.botToken, map[string]any{
		"files":      json.RawMessage(files),
		"channel_id": target,
	}, nil)
}

// chunks splits on line boundaries where possible so long replies stay readable.
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
