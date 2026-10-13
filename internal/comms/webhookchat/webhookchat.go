// Package webhookchat is a chat channel for any platform that speaks incoming
// and outgoing webhooks — Google Chat, Slack, Discord, Microsoft Teams,
// Mattermost, Rocket.Chat, or a bespoke bridge. Adding a chat to KARMAX is a
// few lines of karmax.yaml rather than a new package:
//
//	comms:
//	  channels:
//	    - id: gchat
//	      type: webhook
//	      settings:
//	        preset: googlechat
//	        url: https://chat.googleapis.com/v1/spaces/AAA/messages?key=…&token=…
//	        url.spaces/BBB: https://chat.googleapis.com/v1/spaces/BBB/messages?…
//	        inbound_token: s3cret
//
// Outbound posts go to the target's webhook URL (a named "url.<target>"
// setting, a literal https URL, or the default "url"). Inbound messages arrive
// on POST /comms/<id> (or settings.inbound_path) and are understood in the
// preset's shape, or as plain {"text","user","channel","thread"} JSON.
package webhookchat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
	"go.uber.org/zap"
)

// Presets name the JSON shapes platforms expect.
const (
	PresetGeneric    = "generic"
	PresetGoogleChat = "googlechat"
	PresetSlack      = "slack"
	PresetDiscord    = "discord"
	PresetTeams      = "teams"
	PresetMattermost = "mattermost"
)

// Channel is one webhook-backed chat.
type Channel struct {
	id       string
	preset   string
	settings map[string]string
	token    string
	http     *http.Client
	in       chan comms.Message
	log      *zap.Logger
}

// New builds a channel from its karmax.yaml settings.
func New(id string, settings map[string]string, log *zap.Logger) *Channel {
	if settings == nil {
		settings = map[string]string{}
	}
	preset := strings.ToLower(strings.TrimSpace(settings["preset"]))
	if preset == "" {
		preset = PresetGeneric
	}
	return &Channel{
		id: id, preset: preset, settings: settings,
		token: settings["inbound_token"],
		http:  &http.Client{Timeout: 15 * time.Second},
		in:    make(chan comms.Message, 64),
		log:   log,
	}
}

func (c *Channel) ID() string                      { return c.id }
func (c *Channel) Type() string                    { return "webhook:" + c.preset }
func (c *Channel) Start(ctx context.Context) error { return nil }
func (c *Channel) Stop() error                     { return nil }

func (c *Channel) IncomingMessages() <-chan comms.Message { return c.in }

// InboundPath is where this channel's inbound handler is mounted.
func (c *Channel) InboundPath() string {
	if p := c.settings["inbound_path"]; p != "" {
		return p
	}
	return "/comms/" + c.id
}

// Configured reports whether the channel can send anywhere at all.
func (c *Channel) Configured() bool {
	if c.settings["url"] != "" {
		return true
	}
	for k := range c.settings {
		if strings.HasPrefix(k, "url.") {
			return true
		}
	}
	return false
}

// urlFor resolves a target to the webhook URL that posts there.
func (c *Channel) urlFor(target string) (string, error) {
	target = strings.TrimSpace(target)
	if strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://") {
		return target, nil
	}
	if target != "" {
		if u := c.settings["url."+target]; u != "" {
			return u, nil
		}
		if u := c.settings["url."+strings.TrimPrefix(target, "#")]; u != "" {
			return u, nil
		}
	}
	if u := c.settings["url"]; u != "" {
		return u, nil
	}
	return "", &comms.UnresolvedTargetError{Target: target, Reason: "no webhook url configured for it"}
}

func (c *Channel) Send(ctx context.Context, target, content string) error {
	_, err := c.post(ctx, target, "", content)
	return err
}

// PostThread keeps a case in one thread where the platform can (Google Chat
// threadKey); elsewhere it posts plainly and returns no thread id.
func (c *Channel) PostThread(ctx context.Context, target, thread, text string) (string, error) {
	return c.post(ctx, target, thread, text)
}

func (c *Channel) post(ctx context.Context, target, thread, content string) (string, error) {
	u, err := c.urlFor(target)
	if err != nil {
		return "", err
	}
	body, threadID, u := c.render(u, target, thread, content)
	for _, chunk := range body {
		b, _ := json.Marshal(chunk)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		resp, err := c.http.Do(req)
		if err != nil {
			return "", fmt.Errorf("%s: %w", c.id, err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return "", fmt.Errorf("%s: webhook answered HTTP %d: %s", c.id, resp.StatusCode, strings.TrimSpace(string(raw)))
		}
	}
	return threadID, nil
}

// render shapes a message for the preset. It may split a long message (Discord
// caps at 2000 characters) and may add a thread parameter to the URL.
func (c *Channel) render(u, target, thread, content string) ([]map[string]any, string, string) {
	switch c.preset {
	case PresetGoogleChat:
		key := thread
		if key == "" {
			key = newKey()
		}
		u = withQuery(u, map[string]string{"threadKey": key, "messageReplyOption": "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"})
		return []map[string]any{{"text": content}}, key, u
	case PresetDiscord:
		var out []map[string]any
		for _, part := range split(content, 1900) {
			out = append(out, map[string]any{"content": part})
		}
		return out, "", u
	case PresetSlack, PresetMattermost:
		m := map[string]any{"text": content}
		if thread != "" {
			m["thread_ts"] = thread
		}
		return []map[string]any{m}, "", u
	case PresetTeams:
		return []map[string]any{{"text": strings.ReplaceAll(content, "\n", "  \n")}}, "", u
	}
	m := map[string]any{"text": content, "channel": c.id}
	if target != "" && !strings.HasPrefix(target, "http") {
		m["target"] = target
	}
	if thread != "" {
		m["thread"] = thread
	}
	return []map[string]any{m}, "", u
}

func (c *Channel) SendEmbed(ctx context.Context, target string, e comms.Embed) error {
	var b strings.Builder
	if e.Title != "" {
		b.WriteString("*" + e.Title + "*\n")
	}
	if e.Description != "" {
		b.WriteString(e.Description + "\n")
	}
	for _, f := range e.Fields {
		fmt.Fprintf(&b, "• %s: %s\n", f.Name, f.Value)
	}
	if e.Footer != "" {
		b.WriteString("_" + e.Footer + "_")
	}
	return c.Send(ctx, target, strings.TrimSpace(b.String()))
}

func (c *Channel) SendFile(ctx context.Context, target, filename string, data []byte) error {
	return c.Send(ctx, target, fmt.Sprintf("📎 %s (%d bytes) — files are not supported on this channel", filename, len(data)))
}

// HandleInbound is the HTTP handler for messages arriving from the platform.
func (c *Channel) HandleInbound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if c.token != "" && r.Header.Get("X-Karmax-Token") != c.token && r.URL.Query().Get("token") != c.token &&
		strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != c.token {
		http.Error(w, "unauthorised", http.StatusUnauthorized)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	msg, ok := c.parse(raw, r.Header.Get("Content-Type"))
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		// Platform pings (Google Chat ADDED_TO_SPACE, Slack url_verification) are not messages.
		if ch := challenge(raw); ch != "" {
			_ = json.NewEncoder(w).Encode(map[string]string{"challenge": ch})
			return
		}
		_, _ = w.Write([]byte(`{}`))
		return
	}
	select {
	case c.in <- msg:
	default:
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte(`{}`))
}

func (c *Channel) parse(raw []byte, contentType string) (comms.Message, bool) {
	msg := comms.Message{ChannelType: c.Type(), Direction: comms.Inbound, Timestamp: time.Now(), Metadata: map[string]any{}}
	// Slack/Mattermost outgoing webhooks are form-encoded.
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		v, err := url.ParseQuery(string(raw))
		if err != nil || v.Get("text") == "" {
			return msg, false
		}
		msg.Content, msg.SenderName, msg.SenderID = v.Get("text"), v.Get("user_name"), v.Get("user_id")
		msg.ChannelID = first(v.Get("channel_name"), v.Get("channel_id"))
		msg.ID = first(v.Get("post_id"), v.Get("timestamp"), newKey())
		return msg, true
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return msg, false
	}
	// Google Chat app event.
	if gm, ok := m["message"].(map[string]any); ok {
		if t := s(m, "type"); t != "" && t != "MESSAGE" {
			return msg, false
		}
		sender, _ := gm["sender"].(map[string]any)
		space, _ := gm["space"].(map[string]any)
		if space == nil {
			space, _ = m["space"].(map[string]any)
		}
		thread, _ := gm["thread"].(map[string]any)
		msg.Content = strings.TrimSpace(first(s(gm, "argumentText"), s(gm, "text")))
		msg.SenderName, msg.SenderID = s(sender, "displayName"), s(sender, "name")
		msg.ChannelID = s(space, "name")
		msg.ReplyToID = s(thread, "name")
		msg.ID = first(s(gm, "name"), newKey())
		if s(sender, "type") == "BOT" {
			return msg, false
		}
		return msg, msg.Content != ""
	}
	// Generic, and Slack/Discord-shaped JSON relays.
	msg.Content = strings.TrimSpace(first(s(m, "text"), s(m, "content"), s(m, "message")))
	msg.SenderName = first(s(m, "user"), s(m, "user_name"), s(m, "sender"), s(m, "username"), s(m, "author"))
	msg.SenderID = first(s(m, "user_id"), s(m, "sender_id"), msg.SenderName)
	msg.ChannelID = first(s(m, "channel"), s(m, "target"), s(m, "space"), s(m, "channel_id"))
	msg.ReplyToID = first(s(m, "thread"), s(m, "thread_ts"))
	msg.ID = first(s(m, "id"), s(m, "message_id"), newKey())
	if b, _ := m["bot"].(bool); b {
		return msg, false
	}
	return msg, msg.Content != ""
}

func challenge(raw []byte) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return s(m, "challenge")
}

func s(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func first(v ...string) string {
	for _, x := range v {
		if x != "" {
			return x
		}
	}
	return ""
}

func newKey() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func withQuery(u string, add map[string]string) string {
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	q := p.Query()
	for k, v := range add {
		q.Set(k, v)
	}
	p.RawQuery = q.Encode()
	return p.String()
}

func split(s string, n int) []string {
	r := []rune(s)
	if len(r) <= n {
		return []string{s}
	}
	var out []string
	for len(r) > n {
		cut := n
		for i := n; i > n/2; i-- {
			if r[i] == '\n' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		r = r[cut:]
	}
	return append(out, string(r))
}
