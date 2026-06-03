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
	pollInterval     = 5 * time.Second
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
	id            string
	wacliPath     string
	targetChat    string
	inbox         chan comms.Message
	log           *zap.Logger
	ctx           context.Context
	cancel        context.CancelFunc
	lastMessageID string
	mu            sync.RWMutex
}

// New creates a WhatsAppChannel with the given ID, wacli binary path, and target chat.
func New(id string, wacliPath string, targetChat string, log *zap.Logger) *WhatsAppChannel {
	if wacliPath == "" {
		wacliPath = defaultWacliPath
	}
	return &WhatsAppChannel{
		id:         id,
		wacliPath:  wacliPath,
		targetChat: targetChat,
		inbox:      make(chan comms.Message, 256),
		log:        log,
	}
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

	go w.pollMessages()

	w.log.Info("whatsapp channel started",
		zap.String("channel_id", w.id),
		zap.String("target_chat", w.targetChat),
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

	args := []string{"messages", "--chat", w.targetChat, "--limit", "5"}

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

	// Find new messages: iterate from oldest to newest. Messages are assumed
	// to be returned newest-first, so we reverse before processing.
	reversed := make([]wacliMessage, len(messages))
	for i, m := range messages {
		reversed[len(messages)-1-i] = m
	}

	foundLast := lastID == ""
	for _, msg := range reversed {
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

	// If we never found lastID in the batch (e.g. first run or messages
	// rotated out), update to the newest message to avoid re-processing.
	if !foundLast && lastID != "" {
		w.mu.Lock()
		w.lastMessageID = messages[0].ID
		w.mu.Unlock()
	}

	// On first poll, just record the newest ID without dispatching.
	if lastID == "" && len(messages) > 0 {
		w.mu.Lock()
		w.lastMessageID = messages[0].ID
		w.mu.Unlock()
	}
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
