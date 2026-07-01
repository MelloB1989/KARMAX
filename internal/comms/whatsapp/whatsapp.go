package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	defaultWacliPath = "/home/mellob/code/wacli/wacli"
	pollInterval     = 30 * time.Second
	sendTimeout      = 30 * time.Second
	pollTimeout      = 15 * time.Second
)

// wacliMessage represents a single message returned by wacli messages.
// Fields are deliberately flexible to handle varying JSON schemas.
type wacliMessage struct {
	ID        string `json:"id"`
	Chat      string `json:"chat"`
	Sender    string `json:"sender"`
	Content   string `json:"content"`
	Text      string `json:"text"`
	Timestamp any    `json:"timestamp"`
	Type      string `json:"type"`
	From      string `json:"from"`
	FromMe    bool   `json:"from_me"`
}

// body returns the message body, preferring content over text.
func (m *wacliMessage) body() string {
	if m.Content != "" {
		return m.Content
	}
	return m.Text
}

// senderID returns the sender identifier, preferring sender over from.
func (m *wacliMessage) senderID() string {
	if m.Sender != "" {
		return m.Sender
	}
	return m.From
}

// parsedTimestamp attempts to extract a time.Time from the timestamp field.
func (m *wacliMessage) parsedTimestamp() time.Time {
	switch v := m.Timestamp.(type) {
	case float64:
		return time.Unix(int64(v), 0)
	case string:
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
		if t, err := time.Parse("2006-01-02T15:04:05Z", v); err == nil {
			return t
		}
	}
	return time.Now()
}

// WhatsAppChannel implements comms.Channel for WhatsApp via the local wacli binary.
type WhatsAppChannel struct {
	id         string
	wacliPath  string
	targetChat string
	// commandChat, when set, is the operator's dedicated KARMAX chat: the
	// operator's OWN messages there route to the agent (a command channel) and
	// the agent replies there. KARMAX's own replies are skipped (sentIDs) so it
	// doesn't answer itself.
	commandChat   string
	inbox         chan comms.Message
	log           *zap.Logger
	ctx           context.Context
	cancel        context.CancelFunc
	lastMessageID string
	lastCommandID string
	sentIDs       map[string]struct{}
	mu            sync.RWMutex
}

// New creates a WhatsAppChannel. targetChat is monitored for others' messages;
// commandChat (optional) is the operator's command channel.
func New(id, wacliPath, targetChat, commandChat string, log *zap.Logger) *WhatsAppChannel {
	if wacliPath == "" {
		wacliPath = defaultWacliPath
	}
	return &WhatsAppChannel{
		id:          id,
		wacliPath:   wacliPath,
		targetChat:  targetChat,
		commandChat: commandChat,
		inbox:       make(chan comms.Message, 256),
		sentIDs:     make(map[string]struct{}),
		log:         log,
	}
}

// recordSent remembers a message ID KARMAX sent, so the command-chat poll skips
// KARMAX's own replies (which are from-me) and doesn't answer itself.
func (w *WhatsAppChannel) recordSent(id string) {
	if id == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.sentIDs) > 500 { // keep the set bounded; old ids are past lastCommandID
		w.sentIDs = make(map[string]struct{})
	}
	w.sentIDs[id] = struct{}{}
}

func (w *WhatsAppChannel) wasSent(id string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	_, ok := w.sentIDs[id]
	return ok
}

func (w *WhatsAppChannel) ID() string   { return w.id }
func (w *WhatsAppChannel) Type() string { return "whatsapp" }

// Start verifies the wacli binary, checks daemon status, and begins polling
// for incoming messages.
func (w *WhatsAppChannel) Start(ctx context.Context) error {
	// Verify wacli binary exists.
	if _, err := os.Stat(w.wacliPath); os.IsNotExist(err) {
		return fmt.Errorf("wacli binary not found at %s", w.wacliPath)
	}

	// Check daemon status.
	statusCtx, statusCancel := context.WithTimeout(ctx, 10*time.Second)
	defer statusCancel()

	out, err := exec.CommandContext(statusCtx, w.wacliPath, "status").CombinedOutput()
	if err != nil {
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

	// Seed lastMessageID so we only process messages that arrive AFTER startup.
	w.seedLastMessageID(ctx)

	go w.pollMessages()

	// If a command chat is configured, poll it for the operator's own messages
	// (the KARMAX command channel), skipping KARMAX's own replies.
	if w.commandChat != "" {
		w.seedLastCommandID(ctx)
		go w.pollCommandChat()
	}

	w.log.Info("whatsapp channel started",
		zap.String("channel_id", w.id),
		zap.String("target_chat", w.targetChat),
		zap.String("command_chat", w.commandChat),
		zap.String("wacli_path", w.wacliPath),
	)
	return nil
}

// Stop cancels the polling goroutine.
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

	// Split long messages at WhatsApp's practical limit (~65536 chars).
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
		// Record the sent message ID so the command-chat poll skips our own reply.
		var resp struct {
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
		}
		if json.Unmarshal(out, &resp) == nil {
			w.recordSent(resp.Message.ID)
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

	// Write to a temp file preserving the original extension.
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

// seedLastMessageID does a silent poll to find the latest message ID so that
// the first real poll only picks up messages arriving AFTER startup.
func (w *WhatsAppChannel) seedLastMessageID(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	args := []string{"messages", "--chat", w.targetChat, "--limit", "1"}
	out, err := exec.CommandContext(pollCtx, w.wacliPath, args...).CombinedOutput()
	if err != nil {
		w.log.Warn("failed to seed lastMessageID", zap.Error(err))
		return
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return
	}

	var messages []wacliMessage
	if err := json.Unmarshal([]byte(trimmed), &messages); err != nil {
		var wrapper struct {
			Messages []wacliMessage `json:"messages"`
		}
		if err2 := json.Unmarshal([]byte(trimmed), &wrapper); err2 != nil {
			return
		}
		messages = wrapper.Messages
	}

	if len(messages) > 0 {
		w.mu.Lock()
		w.lastMessageID = messages[0].ID
		w.mu.Unlock()
		w.log.Info("seeded lastMessageID", zap.String("id", messages[0].ID))
	}
}

// pollMessages runs a loop that polls wacli for new messages every pollInterval.
func (w *WhatsAppChannel) pollMessages() {
	w.mu.RLock()
	ctx := w.ctx
	w.mu.RUnlock()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.fetchAndDispatch(ctx)
		}
	}
}

// fetchAndDispatch runs wacli messages, parses the output, and pushes new
// messages to the inbox channel.
func (w *WhatsAppChannel) fetchAndDispatch(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	args := []string{"messages", "--chat", w.targetChat, "--limit", "5", "--from-me", "no"}

	cmd := exec.CommandContext(pollCtx, w.wacliPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.log.Warn("wacli messages poll failed",
			zap.String("channel_id", w.id),
			zap.String("output", string(out)),
			zap.Error(err),
		)
		return
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return
	}

	var messages []wacliMessage
	if err := json.Unmarshal([]byte(trimmed), &messages); err != nil {
		// Try parsing as a JSON object with a messages array.
		var wrapper struct {
			Messages []wacliMessage `json:"messages"`
		}
		if err2 := json.Unmarshal([]byte(trimmed), &wrapper); err2 != nil {
			w.log.Warn("failed to parse wacli messages output",
				zap.String("channel_id", w.id),
				zap.String("raw", trimmed),
				zap.Error(err),
			)
			return
		}
		messages = wrapper.Messages
	}

	if len(messages) == 0 {
		return
	}

	w.mu.RLock()
	lastID := w.lastMessageID
	w.mu.RUnlock()

	// wacli returns the N most recent messages in ascending (oldest→newest)
	// order, so we process them in place — no reversing.
	newestID := messages[len(messages)-1].ID

	// First poll with no baseline: record the newest and dispatch nothing so we
	// don't replay the backlog. (seedLastMessageID normally prevents this.)
	if lastID == "" {
		w.mu.Lock()
		w.lastMessageID = newestID
		w.mu.Unlock()
		return
	}

	// Skip everything up to and including the last message we've seen, then
	// dispatch what's newer.
	foundLast := false
	for _, msg := range messages {
		if !foundLast {
			if msg.ID == lastID {
				foundLast = true
			}
			continue
		}

		// Skip messages from self.
		if msg.FromMe {
			continue
		}

		// Skip empty messages.
		if strings.TrimSpace(msg.body()) == "" {
			continue
		}

		cm := comms.Message{
			ID:          uuid.New().String(),
			ChannelID:   w.targetChat,
			ChannelType: "whatsapp",
			SenderID:    msg.senderID(),
			SenderName:  msg.senderID(),
			Content:     msg.body(),
			Direction:   comms.Inbound,
			Metadata: map[string]any{
				"wacli_message_id": msg.ID,
				"chat":             msg.Chat,
				"message_type":     msg.Type,
			},
			Timestamp: msg.parsedTimestamp(),
		}

		select {
		case w.inbox <- cm:
		default:
			w.log.Warn("whatsapp inbox full, dropping message",
				zap.String("channel_id", w.id),
				zap.String("wacli_msg_id", msg.ID),
			)
		}

		// Update last seen.
		w.mu.Lock()
		w.lastMessageID = msg.ID
		w.mu.Unlock()
	}

	// If we never found lastID in the batch (it rotated out of the window),
	// jump to the newest so we don't replay the whole batch next time.
	if !foundLast {
		w.mu.Lock()
		w.lastMessageID = newestID
		w.mu.Unlock()
	}
}

// seedLastCommandID records the newest message in the command chat so we only
// process messages that arrive after startup.
func (w *WhatsAppChannel) seedLastCommandID(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	out, err := exec.CommandContext(pollCtx, w.wacliPath, "messages", "--chat", w.commandChat, "--limit", "1").CombinedOutput()
	if err != nil {
		return
	}
	msgs := parseWacliMessages(strings.TrimSpace(string(out)))
	if len(msgs) > 0 {
		w.mu.Lock()
		w.lastCommandID = msgs[0].ID
		w.mu.Unlock()
	}
}

// pollCommandChat polls the command chat for the operator's messages.
func (w *WhatsAppChannel) pollCommandChat() {
	w.mu.RLock()
	ctx := w.ctx
	w.mu.RUnlock()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.fetchCommandMessages(ctx)
		}
	}
}

// fetchCommandMessages polls the command chat (including the operator's own
// messages), skips KARMAX's own replies, and dispatches the rest to the agent.
func (w *WhatsAppChannel) fetchCommandMessages(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	// No --from-me filter: we WANT the operator's own messages here.
	out, err := exec.CommandContext(pollCtx, w.wacliPath, "messages", "--chat", w.commandChat, "--limit", "5").CombinedOutput()
	if err != nil {
		w.log.Warn("wacli command-chat poll failed", zap.String("channel_id", w.id), zap.Error(err))
		return
	}
	messages := parseWacliMessages(strings.TrimSpace(string(out)))
	if len(messages) == 0 {
		return
	}

	w.mu.RLock()
	lastID := w.lastCommandID
	w.mu.RUnlock()

	// wacli returns the N most recent messages oldest→newest, so process in
	// place (no reversing).
	newestID := messages[len(messages)-1].ID

	// First poll with no baseline: record the newest and dispatch nothing so we
	// don't replay history. (seedLastCommandID normally prevents this.)
	if lastID == "" {
		w.mu.Lock()
		w.lastCommandID = newestID
		w.mu.Unlock()
		return
	}

	foundLast := false
	for _, msg := range messages {
		if !foundLast {
			if msg.ID == lastID {
				foundLast = true
			}
			continue
		}
		// Skip KARMAX's own replies (from-me but sent by us) and empties.
		if w.wasSent(msg.ID) || strings.TrimSpace(msg.body()) == "" {
			w.mu.Lock()
			w.lastCommandID = msg.ID
			w.mu.Unlock()
			continue
		}
		cm := comms.Message{
			ID:          uuid.New().String(),
			ChannelID:   w.commandChat, // reply goes back here
			ChannelType: "whatsapp",
			SenderID:    msg.senderID(),
			SenderName:  msg.senderID(),
			Content:     msg.body(),
			Direction:   comms.Inbound,
			Metadata: map[string]any{
				"wacli_message_id": msg.ID,
				"chat":             msg.Chat,
				"command_chat":     true,
			},
			Timestamp: msg.parsedTimestamp(),
		}
		select {
		case w.inbox <- cm:
		default:
			w.log.Warn("whatsapp inbox full, dropping command message", zap.String("channel_id", w.id))
		}
		w.mu.Lock()
		w.lastCommandID = msg.ID
		w.mu.Unlock()
	}

	// lastID rotated out of the window — jump to newest to avoid replaying.
	if !foundLast {
		w.mu.Lock()
		w.lastCommandID = newestID
		w.mu.Unlock()
	}
}

// parseWacliMessages parses `wacli messages` output (array or {messages:[...]}).
func parseWacliMessages(trimmed string) []wacliMessage {
	if trimmed == "" {
		return nil
	}
	var messages []wacliMessage
	if err := json.Unmarshal([]byte(trimmed), &messages); err != nil {
		var wrapper struct {
			Messages []wacliMessage `json:"messages"`
		}
		if json.Unmarshal([]byte(trimmed), &wrapper) != nil {
			return nil
		}
		messages = wrapper.Messages
	}
	return messages
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
