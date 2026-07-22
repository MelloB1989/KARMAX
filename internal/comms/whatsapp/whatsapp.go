package whatsapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	sendTimeout     = 30 * time.Second
	statusTimeout   = 10 * time.Second
	wacliCmdTimeout = 15 * time.Second
)

// wacliWebhookEnvelope is the JSON body wacli POSTs to our webhook endpoint.
// See wacli service.go buildMessageWebhookPayload / dispatchWebhook.
type wacliWebhookEnvelope struct {
	Event   string              `json:"event"`
	Payload wacliWebhookPayload `json:"payload"`
}

type wacliWebhookPayload struct {
	Chat    wacliChat    `json:"chat"`
	Message wacliMessage `json:"message"`
	// Source distinguishes the operator's real WhatsApp activity
	// ("whatsapp_event") from messages KARMAX itself sent through the wacli API
	// ("wacli_api"). This is how we avoid answering our own replies.
	Source string `json:"source"`
}

type wacliChat struct {
	JID     string `json:"jid"`
	Name    string `json:"name"`
	Locked  bool   `json:"locked"`
	IsGroup bool   `json:"is_group"`
}

// wacliMessage mirrors wacli's MessageRecord JSON tags.
type wacliMessage struct {
	ID          string    `json:"id"`
	ChatJID     string    `json:"chat_jid"`
	SenderJID   string    `json:"sender_jid"`
	Content     string    `json:"content"`
	Timestamp   time.Time `json:"timestamp"`
	IsFromMe    bool      `json:"is_from_me"`
	MessageType string    `json:"message_type"`
	// MentionsMe: this message @-mentions the KARMAX/bot account. QuotedIsFromMe:
	// it's a reply to a message KARMAX sent. wacli computes both from its own
	// identity (generic — no configured numbers), letting the proxy know KARMAX
	// is being directly addressed even without an explicit @-mention.
	MentionsMe     bool `json:"mentions_me"`
	QuotedIsFromMe bool `json:"quoted_is_from_me"`
	// MentionCount = how many JIDs the message @-mentioned, so the proxy loop
	// can tell a direct mention from an "@all"-style blast.
	MentionCount int `json:"mention_count"`
}

// WhatsAppChannel implements comms.Channel for WhatsApp via the local wacli
// binary. It is EVENT-BASED and a pure CONSUMER: it exposes an HTTP endpoint
// (HandleWebhook) that the wacli webhook posts message events to, and it replies
// to whichever chat the event came from. It does NOT register or scope the wacli
// webhook itself — that lives in wacli (managed via the `wacli` agent tool or
// `wacli webhooks` CLI), so no chat JIDs are hardcoded here.
type WhatsAppChannel struct {
	id            string
	wacliPath     string
	targetChat    string // default send target (e.g. for briefings); not a filter
	webhookSecret string // optional HMAC secret; must match the wacli webhook's --secret
	inbox         chan comms.Message
	log           *zap.Logger
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.RWMutex
}

// New creates a WhatsAppChannel. targetChat is only the default send target.
// webhookSecret (optional) is verified against the wacli webhook's HMAC; leave
// empty to skip verification (fine for a localhost-only endpoint).
func New(id, wacliPath, targetChat, webhookSecret string, log *zap.Logger) *WhatsAppChannel {
	if wacliPath == "" {
		wacliPath = hostpaths.Wacli()
	}
	return &WhatsAppChannel{
		id:            id,
		wacliPath:     wacliPath,
		targetChat:    targetChat,
		webhookSecret: webhookSecret,
		inbox:         make(chan comms.Message, 256),
		log:           log,
	}
}

func (w *WhatsAppChannel) ID() string   { return w.id }
func (w *WhatsAppChannel) Type() string { return "whatsapp" }

// Start verifies the wacli binary and daemon, then registers the wacli webhook
// so message events are pushed to KARMAX (event-based; no polling).
func (w *WhatsAppChannel) Start(ctx context.Context) error {
	if _, err := os.Stat(w.wacliPath); os.IsNotExist(err) {
		return fmt.Errorf("wacli binary not found at %s", w.wacliPath)
	}

	statusCtx, statusCancel := context.WithTimeout(ctx, statusTimeout)
	defer statusCancel()
	if out, err := exec.CommandContext(statusCtx, w.wacliPath, "status").CombinedOutput(); err != nil {
		w.log.Warn("wacli status check failed, proceeding anyway",
			zap.String("channel_id", w.id),
			zap.String("output", string(out)),
			zap.Error(err),
		)
	} else {
		w.log.Info("wacli daemon status",
			zap.String("channel_id", w.id),
			zap.String("status", strings.TrimSpace(string(out))),
		)
	}

	w.mu.Lock()
	w.ctx, w.cancel = context.WithCancel(ctx)
	w.mu.Unlock()

	w.log.Info("whatsapp channel started (webhook consumer)",
		zap.String("channel_id", w.id),
		zap.String("target_chat", w.targetChat),
		zap.Bool("hmac", w.webhookSecret != ""),
		zap.String("wacli_path", w.wacliPath),
	)
	return nil
}

// HandleWebhook is the HTTP entry point wacli POSTs message events to. It
// verifies the HMAC signature and routes qualifying events to the agent.
func (w *WhatsAppChannel) HandleWebhook(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(rw, "read body", http.StatusBadRequest)
		return
	}
	if w.webhookSecret != "" && !verifyWacliSignature(body, w.webhookSecret, r.Header.Get("X-WACLI-Signature")) {
		w.log.Warn("wacli webhook signature mismatch", zap.String("channel_id", w.id))
		http.Error(rw, "invalid signature", http.StatusUnauthorized)
		return
	}

	var env wacliWebhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(rw, "bad json", http.StatusBadRequest)
		return
	}

	// Log real activity (skip our own sends, which are noise).
	if env.Payload.Source != "wacli_api" {
		w.log.Info("wacli webhook received",
			zap.String("channel_id", w.id),
			zap.String("event", env.Event),
			zap.String("chat", normalizeJID(env.Payload.Chat.JID)),
		)
	}

	// Ack immediately; routing is cheap and non-blocking.
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte(`{"ok":true}`))

	w.routeEvent(env)
}

// routeEvent decides whether a wacli event should reach the agent and, if so,
// emits an inbound comms message whose ChannelID is the FULL origin JID — so the
// agent's reply goes back to the exact chat the message came from (which handles
// the "@lid" vs "@s.whatsapp.net" split transparently).
func (w *WhatsAppChannel) routeEvent(env wacliWebhookEnvelope) {
	msg := env.Payload.Message
	chatJID := strings.TrimSpace(env.Payload.Chat.JID)
	if chatJID == "" {
		chatJID = strings.TrimSpace(msg.ChatJID)
	}
	body := strings.TrimSpace(msg.Content)
	// Media (image/PDF/Excel/doc/video/voice) arrives with an empty text body.
	// Instead of dropping it, attach a marker so the agent knows a file came in
	// and can call whatsapp.view_media to actually see/read it. Any caption on
	// the media is preserved alongside the marker.
	if mediaLabel := whatsappMediaLabel(msg.MessageType); mediaLabel != "" {
		marker := fmt.Sprintf("[received a %s — to see/read it, call whatsapp.view_media with chat=%q and message_id=%q]",
			mediaLabel, chatJID, msg.ID)
		if body == "" {
			body = marker
		} else {
			body = body + "\n" + marker
		}
	}
	if body == "" {
		return // truly empty / non-actionable (reaction, protocol message)
	}

	// No chat filtering here: which chats reach us is decided entirely by the
	// wacli webhook's scope (managed in wacli). We reply to whichever chat sent
	// the event.
	switch env.Event {
	case "incoming_message":
		// A message from the other party — always route.
	case "outgoing_message":
		// The operator's OWN message (e.g. their self-chat, or a phone that is
		// the wacli account). Skip our own sends (source "wacli_api") so we never
		// answer ourselves.
		if env.Payload.Source == "wacli_api" {
			return
		}
	default:
		return
	}

	ts := msg.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	cm := comms.Message{
		ID:          uuid.New().String(),
		ChannelID:   chatJID, // reply goes back to the originating chat
		ChannelType: "whatsapp",
		SenderID:    normalizeJID(msg.SenderJID),
		SenderName:  env.Payload.Chat.Name,
		Content:     body,
		Direction:   comms.Inbound,
		Metadata: map[string]any{
			"wacli_message_id":  msg.ID,
			"chat_jid":          chatJID,
			"event":             env.Event,
			"source":            env.Payload.Source,
			"is_group":          env.Payload.Chat.IsGroup,
			"chat_name":         env.Payload.Chat.Name,
			"mentions_me":       msg.MentionsMe,
			"quoted_is_from_me": msg.QuotedIsFromMe,
			"mention_count":     msg.MentionCount,
		},
		Timestamp: ts,
	}

	select {
	case w.inbox <- cm:
		w.log.Info("whatsapp event dispatched to agent",
			zap.String("channel_id", w.id),
			zap.String("event", env.Event),
			zap.String("chat", chatJID),
		)
	default:
		w.log.Warn("whatsapp inbox full, dropping event", zap.String("channel_id", w.id))
	}
}

// verifyWacliSignature checks the "sha256=<hex>" X-WACLI-Signature header.
func verifyWacliSignature(body []byte, secret, signature string) bool {
	signature = strings.TrimPrefix(strings.TrimSpace(signature), "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// normalizeJID reduces a JID or phone to bare digits for matching, stripping any
// "@domain" and ":device" suffix. "15551234567@s.whatsapp.net" -> "15551234567".
func normalizeJID(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "@:"); i >= 0 {
		s = s[:i]
	}
	return s
}

// Stop cancels the channel context.
func (w *WhatsAppChannel) Stop() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		w.cancel()
	}
	return nil
}

// Send sends a text message to the given target (JID or phone number).
// If target is empty, the configured targetChat is used.
func (w *WhatsAppChannel) Send(ctx context.Context, target, content string) error {
	if strings.TrimSpace(content) == "" {
		w.log.Debug("skipping empty whatsapp message",
			zap.String("channel_id", w.id),
			zap.String("target", target),
		)
		return nil
	}

	if target == "" {
		target = w.targetChat
	}
	if target == "" {
		return fmt.Errorf("no target specified and no default target_chat configured")
	}

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	// Split long messages at WhatsApp's practical limit.
	chunks := splitContent(content, 4096)
	for _, chunk := range chunks {
		cmd := exec.CommandContext(sendCtx, w.wacliPath, "send", "--to", target, "--text", chunk)
		out, err := cmd.CombinedOutput()
		if err != nil {
			w.log.Error("failed to send whatsapp message",
				zap.String("channel_id", w.id),
				zap.String("target", target),
				zap.String("output", string(out)),
				zap.Error(err),
			)
			return fmt.Errorf("send whatsapp message: %w", err)
		}
	}
	return nil
}

// SendEmbed formats a comms.Embed as plain text and sends it via Send.
// WhatsApp does not support rich embeds natively.
func (w *WhatsAppChannel) SendEmbed(ctx context.Context, target string, embed comms.Embed) error {
	var sb strings.Builder

	if embed.Title != "" {
		sb.WriteString("*")
		sb.WriteString(embed.Title)
		sb.WriteString("*\n")
	}
	if embed.Description != "" {
		sb.WriteString(embed.Description)
		sb.WriteString("\n")
	}
	for _, f := range embed.Fields {
		sb.WriteString("\n*")
		sb.WriteString(f.Name)
		sb.WriteString(":* ")
		sb.WriteString(f.Value)
	}
	if embed.Footer != "" {
		sb.WriteString("\n\n_")
		sb.WriteString(embed.Footer)
		sb.WriteString("_")
	}

	return w.Send(ctx, target, sb.String())
}

// SendFile writes data to a temporary file and sends it via wacli --media flag.
func (w *WhatsAppChannel) SendFile(ctx context.Context, target, filename string, data []byte) error {
	if target == "" {
		target = w.targetChat
	}
	if target == "" {
		return fmt.Errorf("no target specified and no default target_chat configured")
	}

	ext := filepath.Ext(filename)
	tmpFile, err := os.CreateTemp("", "karmax-wa-*"+ext)
	if err != nil {
		return fmt.Errorf("create temp file for whatsapp send: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp file for whatsapp send: %w", err)
	}
	tmpFile.Close()

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	cmd := exec.CommandContext(sendCtx, w.wacliPath, "send", "--to", target, "--media", tmpPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.log.Error("failed to send whatsapp file",
			zap.String("channel_id", w.id),
			zap.String("target", target),
			zap.String("filename", filename),
			zap.String("output", string(out)),
			zap.Error(err),
		)
		return fmt.Errorf("send whatsapp file: %w", err)
	}
	return nil
}

// IncomingMessages returns the read-only channel of inbound messages.
func (w *WhatsAppChannel) IncomingMessages() <-chan comms.Message {
	return w.inbox
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

// whatsappMediaLabel maps a wacli message_type to a human media label, or ""
// for non-media (text, reactions, unknown/protocol messages).
func whatsappMediaLabel(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "image":
		return "image"
	case "document":
		return "document/file (PDF, Excel, doc, etc.)"
	case "video":
		return "video"
	case "audio", "ptt":
		return "voice note/audio"
	case "sticker":
		return "sticker"
	default:
		return ""
	}
}
