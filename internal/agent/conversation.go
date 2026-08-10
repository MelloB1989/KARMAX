package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	gitloom "github.com/GitLoomHQ/gitloom-go/gitloom"
	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/bus"
	"go.uber.org/zap"
)

// The operator's conversations with KARMAX, stored in GitLoom.
//
// SQLite still holds chat_history: that is the model's working window and what
// makes a restart resume mid-thought. This is a different thing — the durable
// record of what the operator and KARMAX actually said to each other, one
// conversation per channel thread, which GitLoom compacts and mines for
// memories on its own cadence.
//
// That last part is the point. Memory used to form only when the agent decided
// to call memory.ingest, so what got remembered depended on the agent noticing
// it was worth remembering. A stored conversation is mined whether anyone
// noticed or not.
//
// THE OPERATOR'S conversations only. Everyone else KARMAX talks to on the
// operator's behalf — the monitored WhatsApp chats — contributes memories and
// nothing more. Storing a third party's whole conversation would be keeping a
// transcript of someone who never agreed to one, to answer questions that the
// memories already answer.

// Conversations records operator conversations, one per channel thread.
type Conversations struct {
	client *gitloom.Client
	ns     string
	model  string
	log    *zap.Logger

	mu     sync.Mutex
	byID   map[string]*gitloom.Conversation
	broken map[string]time.Time
}

// ConversationsConfig configures the recorder.
type ConversationsConfig struct {
	APIKey    string
	BaseURL   string
	Namespace string
	// Model whose context window bounds each stored conversation, so GitLoom
	// compacts at the right point rather than at a guess.
	Model string
}

// NewConversations builds a recorder, or nil when GitLoom is not configured —
// which is the self-hosted default and not an error.
func NewConversations(cfg ConversationsConfig, log *zap.Logger) *Conversations {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil
	}
	opts := []gitloom.Option{gitloom.WithNamespace(cfg.Namespace)}
	if cfg.BaseURL != "" {
		opts = append(opts, gitloom.WithBaseURL(cfg.BaseURL))
	}
	return &Conversations{
		client: gitloom.New(cfg.APIKey, opts...),
		ns:     cfg.Namespace,
		model:  cfg.Model,
		log:    log,
		byID:   map[string]*gitloom.Conversation{},
		broken: map[string]time.Time{},
	}
}

// retryBroken is how long a thread that failed to open is left alone. Without
// it, a namespace GitLoom will not accept costs a failed round trip on every
// single message the operator sends.
const retryBroken = 5 * time.Minute

// Record appends one exchange to the thread's stored conversation.
//
// Best-effort by design: this is the archive, not the reply path. A failure
// here must never cost the operator their answer, so it is logged and the turn
// carries on.
func (c *Conversations) Record(ctx context.Context, threadID, title, userMsg, reply string) {
	if c == nil || strings.TrimSpace(threadID) == "" {
		return
	}
	if strings.TrimSpace(userMsg) == "" && strings.TrimSpace(reply) == "" {
		return
	}

	conv, err := c.conversation(ctx, threadID, title)
	if err != nil {
		c.log.Warn("could not open the stored conversation; this exchange is not archived",
			zap.String("thread", threadID), zap.Error(err))
		return
	}

	msgs := make([]models.AIMessage, 0, 2)
	if strings.TrimSpace(userMsg) != "" {
		msgs = append(msgs, models.AIMessage{Role: models.User, Message: userMsg})
	}
	if strings.TrimSpace(reply) != "" {
		msgs = append(msgs, models.AIMessage{Role: models.Assistant, Message: reply})
	}
	if err := conv.Append(ctx, msgs, nil); err != nil {
		c.log.Warn("could not append to the stored conversation",
			zap.String("thread", threadID), zap.Error(err))
		// Dropped from the cache so the next message re-opens it: the handle
		// may be stale, and a stale handle fails identically forever.
		c.mu.Lock()
		delete(c.byID, threadID)
		c.mu.Unlock()
	}
}

// conversation returns the thread's conversation, opening it on first use.
func (c *Conversations) conversation(ctx context.Context, threadID, title string) (*gitloom.Conversation, error) {
	c.mu.Lock()
	if conv, ok := c.byID[threadID]; ok {
		c.mu.Unlock()
		return conv, nil
	}
	if at, ok := c.broken[threadID]; ok && time.Since(at) < retryBroken {
		c.mu.Unlock()
		return nil, fmt.Errorf("this thread failed to open recently; not retrying yet")
	}
	c.mu.Unlock()

	opts := gitloom.ConversationOptions{
		Namespace: c.ns,
		Model:     c.model,
		Title:     title,
		// GitLoom summarises its own compactions. The alternative is handing it
		// a KarmaAI, which would put a model call on the archive path — and the
		// archive must never be able to slow down a reply.
		SummarizeServer: true,
	}

	// Resumed rather than recreated, so a restart continues the thread instead
	// of starting a second one beside it.
	conv, err := c.client.LoadConversation(ctx, threadID, opts)
	if err != nil {
		conv, err = c.client.NewConversation(ctx, threadID, opts)
	}
	if err != nil {
		c.mu.Lock()
		c.broken[threadID] = time.Now()
		c.mu.Unlock()
		return nil, err
	}

	c.mu.Lock()
	c.byID[threadID] = conv
	delete(c.broken, threadID)
	c.mu.Unlock()
	return conv, nil
}

// SetConversations installs the recorder for the operator's conversations.
func (a *Agent) SetConversations(c *Conversations) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conversations = c
}

// recordConversation archives one exchange, when it is one the operator had.
func (a *Agent) recordConversation(ctx context.Context, evt bus.Event, userMsg, reply string) {
	a.mu.RLock()
	c := a.conversations
	a.mu.RUnlock()
	if c == nil {
		return
	}

	thread, title, ok := a.conversationThread(evt)
	if !ok {
		return
	}
	c.Record(ctx, thread, title, userMsg, reply)
}

// conversationThread names the thread an event belongs to, and reports whether
// it should be stored at all.
//
// One conversation per channel thread rather than one per agent: the operator's
// WhatsApp thread and their app thread are different conversations, and merging
// them would produce a transcript that reads as neither.
func (a *Agent) conversationThread(evt bus.Event) (id, title string, ok bool) {
	switch evt.Kind {
	case bus.EventCommsMessage:
		chatID, _ := evt.Payload["channel_id"].(string)
		if chatID == "" {
			return "", "", false
		}
		// The line that keeps third parties out. A monitored chat is someone
		// KARMAX talks to FOR the operator, not a conversation the operator is
		// having, and it contributes memories instead of a transcript.
		if !a.isFromOperator(chatID) {
			return "", "", false
		}
		name, _ := evt.Payload["chat_name"].(string)
		if name == "" {
			name = chatID
		}
		return "chat:" + normalizeChat(chatID), "Conversation with " + name, true

	case "api.chat":
		// The phone app, which is the operator by definition — nobody else has
		// it.
		return "app:" + a.def.ID, "App conversation", true
	}
	return "", "", false
}
