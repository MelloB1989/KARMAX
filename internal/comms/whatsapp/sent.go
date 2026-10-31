package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
)

// What the account itself sent, as wacli knows it.
//
// comms records what it sends; it never saw what was sent through wacli
// directly. wacli does: it echoes every send it makes as an outgoing message
// (source "wacli_api"), and keeps every message in its own history, which can
// be asked by chat, sender and time. The echo makes those sends part of the
// record; the history answers "did this go out" for a send that reached
// WhatsApp before anything here could record it.

var _ comms.EchoSender = (*WhatsAppChannel)(nil)
var _ comms.SentChecker = (*WhatsAppChannel)(nil)

// SetEchoSink is where the account's own wacli sends are reported.
func (w *WhatsAppChannel) SetEchoSink(fn func(comms.Echo)) {
	w.mu.Lock()
	w.echo = fn
	w.mu.Unlock()
}

// echoSent reports one send wacli made, as its echo describes it.
func (w *WhatsAppChannel) echoSent(chatJID string, msg wacliMessage) {
	w.mu.RLock()
	sink := w.echo
	w.mu.RUnlock()
	if sink == nil || strings.TrimSpace(chatJID) == "" {
		return
	}
	content := strings.TrimSpace(msg.Content)
	if name := strings.TrimSpace(msg.FileName); name != "" {
		content = comms.FileMessage(name)
	}
	if content == "" {
		return
	}
	at := msg.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	sink(comms.Echo{Target: chatJID, Content: content, ChannelType: "whatsapp", MessageID: msg.ID, At: at})
}

// SentSince asks wacli's own history whether the account sent content to
// target at or after since. content is a message's text, or
// comms.FileMessage(name) for a file.
func (w *WhatsAppChannel) SentSince(ctx context.Context, target, content string, since time.Time) (bool, error) {
	content = strings.TrimSpace(content)
	if strings.TrimSpace(target) == "" || content == "" {
		return false, nil
	}
	args := []string{"messages", "--chat", target, "--from-me", "yes", "--limit", "50"}
	if !since.IsZero() {
		// A little before, for clocks that disagree by a second or two.
		args = append(args, "--after", since.Add(-time.Minute).UTC().Format(time.RFC3339))
	}
	out, err := exec.CommandContext(ctx, w.wacliPath, args...).Output()
	if err != nil {
		return false, fmt.Errorf("wacli messages: %w", err)
	}
	var res struct {
		Messages []wacliMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return false, fmt.Errorf("wacli messages: %w", err)
	}
	wantFile := strings.HasPrefix(content, comms.FileMessagePrefix)
	for _, m := range res.Messages {
		if strings.TrimSpace(m.Content) == content {
			return true, nil
		}
		if wantFile && m.FileName != "" && comms.FileMessage(m.FileName) == content {
			return true, nil
		}
	}
	return false, nil
}
