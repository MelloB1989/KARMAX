// Package coldscan runs the background "cold" memory pipeline: it walks older
// WhatsApp chats (those the operator is no longer actively using) and generates
// a durable per-chat summary, stored in chat_summaries for the retrieval
// sub-agent to draw on. Active/"hot" chats are left to the foreground sync; very
// large groups the operator barely participates in (community/promo groups) are
// skipped.
package coldscan

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// Config controls the cold-scan worker.
type Config struct {
	Enabled     bool
	Interval    time.Duration
	PerTick     int // chats summarized per tick (rate limit)
	HotDays     int // chats with activity newer than this are "hot" and skipped
	MinGroupOwn int // min of the operator's own messages for a GROUP to be worth summarizing
	WacliPath   string
	Provider    string
	Model       string
	Fallbacks   []karmahelper.FallbackModel
}

// Scanner is the background cold-memory worker.
type Scanner struct {
	cfg   Config
	store *store.Store
	log   *zap.Logger
}

const coldSummaryPrompt = `You write durable memory about one of the operator's contacts based on a WhatsApp conversation. Summarize: who the other party is (relationship/role if inferable), the key topics discussed, any commitments / decisions / deadlines / important facts, and anything genuinely useful to remember later. 2–6 factual sentences, no fluff. If the conversation has no substance worth remembering (spam, one-off, pure logistics with no lasting info), reply with exactly: SKIP`

// New builds a cold-scan worker, applying sensible defaults.
func New(cfg Config, s *store.Store, log *zap.Logger) *Scanner {
	if cfg.Interval <= 0 {
		cfg.Interval = 20 * time.Minute
	}
	if cfg.PerTick <= 0 {
		cfg.PerTick = 4
	}
	if cfg.HotDays <= 0 {
		cfg.HotDays = 14
	}
	if cfg.MinGroupOwn <= 0 {
		cfg.MinGroupOwn = 5
	}
	if cfg.WacliPath == "" {
		cfg.WacliPath = "/home/mellob/code/wacli/wacli"
	}
	return &Scanner{cfg: cfg, store: s, log: log}
}

// Start runs the periodic worker until ctx is cancelled.
func (s *Scanner) Start(ctx context.Context) {
	if !s.cfg.Enabled {
		return
	}
	s.log.Info("cold-scan worker started",
		zap.Duration("interval", s.cfg.Interval), zap.Int("per_tick", s.cfg.PerTick),
		zap.Int("hot_days", s.cfg.HotDays))

	// Small initial delay so we don't compete with startup.
	select {
	case <-ctx.Done():
		return
	case <-time.After(45 * time.Second):
	}
	s.runOnce(ctx)

	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runOnce(ctx)
		}
	}
}

type chatRec struct {
	JID           string    `json:"jid"`
	Name          string    `json:"name"`
	IsGroup       bool      `json:"is_group"`
	Locked        bool      `json:"locked"`
	LastMessageAt time.Time `json:"last_message_at"`
}

type msgRec struct {
	Content   string    `json:"content"`
	IsFromMe  bool      `json:"is_from_me"`
	Timestamp time.Time `json:"timestamp"`
}

// recheckInterval bounds how often a chat is re-examined (one wacli own-message
// lookup per chat per day), keeping the worker cheap across hundreds of chats.
const recheckInterval = 24 * time.Hour

// runOnce examines chats and summarizes the "cold" ones. Hot vs cold is decided
// by the OPERATOR's own last message (not the chat's activity), so a group that
// stays busy with other people but that the operator hasn't texted in for weeks
// correctly becomes cold. Each chat is recorded (summarized | hot | skipped) so
// subsequent ticks skip it cheaply until recheckInterval elapses.
func (s *Scanner) runOnce(ctx context.Context) {
	chats, err := s.listChats(ctx)
	if err != nil {
		s.log.Warn("cold-scan: list chats failed", zap.Error(err))
		return
	}
	cutoff := time.Now().AddDate(0, 0, -s.cfg.HotDays)
	// Process oldest chats first so genuinely-cold conversations (the ones the
	// operator hasn't touched in a long time) get summarized promptly.
	sort.Slice(chats, func(i, j int) bool { return chats[i].LastMessageAt.Before(chats[j].LastMessageAt) })
	summarized, examined := 0, 0
	checkBudget := s.cfg.PerTick * 8
	if checkBudget < 30 {
		checkBudget = 30
	}

	for _, c := range chats {
		if summarized >= s.cfg.PerTick || examined >= checkBudget {
			break
		}
		// Note: we do NOT skip "locked" chats — when wacli access control is
		// unconfigured every chat defaults to locked yet reads work fine.
		// Relevance is decided by the operator's participation below.
		ex, _ := s.store.GetChatSummary(c.JID)
		if ex != nil && time.Since(ex.SummarizedAt) < recheckInterval {
			continue // examined recently
		}

		ownLast, ownCount := s.ownLastMessage(ctx, c.JID)
		examined++

		// No / negligible participation -> not useful memory.
		if ownCount == 0 || (c.IsGroup && ownCount < s.cfg.MinGroupOwn) {
			s.record(c, "skipped", "", ownCount, ownLast, 0)
			continue
		}
		// Operator still active here -> hot; leave it to the foreground sync.
		if ownLast.After(cutoff) {
			s.record(c, "hot", "", ownCount, ownLast, 0)
			continue
		}
		// Cold, but don't re-summarize if nothing changed since last time.
		if ex != nil && ex.Status == "summarized" && !ownLast.After(ex.LastMessageAt) {
			s.record(c, "summarized", ex.Summary, ownCount, ownLast, ex.MessageCount)
			continue
		}
		msgs := s.fetchMessages(ctx, c.JID, 150)
		if len(msgs) < 3 {
			s.record(c, "skipped", "", ownCount, ownLast, len(msgs))
			continue
		}
		summary, ok := s.summarize(ctx, c, msgs)
		if !ok {
			s.record(c, "skipped", "", ownCount, ownLast, len(msgs))
			continue
		}
		s.record(c, "summarized", summary, ownCount, ownLast, len(msgs))
		summarized++
	}
	if summarized > 0 || examined > 0 {
		s.log.Info("cold-scan tick", zap.Int("summarized", summarized), zap.Int("examined", examined))
	}
}

// record upserts a chat's cold-scan state. LastMessageAt stores the operator's
// own last message time (the signal the hot/cold decision is based on).
func (s *Scanner) record(c chatRec, status, summary string, ownCount int, ownLast time.Time, msgCount int) {
	if err := s.store.UpsertChatSummary(store.ChatSummary{
		ChatJID: c.JID, ChatName: c.Name, IsGroup: c.IsGroup,
		Summary: summary, MessageCount: msgCount, OwnMessageCount: ownCount,
		LastMessageAt: ownLast, SummarizedAt: time.Now(), Status: status,
	}); err != nil {
		s.log.Warn("cold-scan: store state failed", zap.String("chat", c.Name), zap.Error(err))
	}
}

func (s *Scanner) listChats(ctx context.Context) ([]chatRec, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, s.cfg.WacliPath, "chats", "--json", "--limit", "1000").Output()
	if err != nil {
		return nil, err
	}
	var chats []chatRec
	if err := json.Unmarshal(out, &chats); err != nil {
		return nil, err
	}
	return chats, nil
}

// ownLastMessage returns the operator's most recent own-message time in a chat
// and a count of their recent own messages (capped by the lookup limit).
func (s *Scanner) ownLastMessage(ctx context.Context, jid string) (time.Time, int) {
	msgs := s.runMessages(ctx, jid, 50, true)
	var last time.Time
	for _, m := range msgs {
		if m.Timestamp.After(last) {
			last = m.Timestamp
		}
	}
	return last, len(msgs)
}

func (s *Scanner) fetchMessages(ctx context.Context, jid string, limit int) []msgRec {
	return s.runMessages(ctx, jid, limit, false)
}

func (s *Scanner) runMessages(ctx context.Context, jid string, limit int, fromMeOnly bool) []msgRec {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"messages", "--chat", jid, "--limit", strconv.Itoa(limit)}
	if fromMeOnly {
		args = append(args, "--from-me", "yes")
	}
	out, err := exec.CommandContext(cctx, s.cfg.WacliPath, args...).Output()
	if err != nil {
		return nil
	}
	return parseMessages(out)
}

// parseMessages handles both {"messages":[...]} and a bare [...] array.
func parseMessages(out []byte) []msgRec {
	var wrap struct {
		Messages []msgRec `json:"messages"`
	}
	if json.Unmarshal(out, &wrap) == nil && len(wrap.Messages) > 0 {
		return wrap.Messages
	}
	var arr []msgRec
	if json.Unmarshal(out, &arr) == nil {
		return arr
	}
	return nil
}

func (s *Scanner) summarize(ctx context.Context, c chatRec, msgs []msgRec) (string, bool) {
	var b strings.Builder
	for _, m := range msgs {
		txt := strings.TrimSpace(strings.ReplaceAll(m.Content, "\n", " "))
		if txt == "" {
			continue
		}
		who := "them"
		if m.IsFromMe {
			who = "me"
		}
		if len(txt) > 220 {
			txt = txt[:220] + "…"
		}
		b.WriteString(who + ": " + txt + "\n")
	}
	transcript := strings.TrimSpace(b.String())
	if transcript == "" {
		return "", false
	}

	kind := "direct chat"
	if c.IsGroup {
		kind = "group"
	}
	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:       s.cfg.Provider,
		Model:          s.cfg.Model,
		SystemPrompt:   coldSummaryPrompt,
		MaxTokens:      700,
		FallbackModels: s.cfg.Fallbacks,
	}, nil)

	prompt := fmt.Sprintf("Conversation with %q (%s). Recent messages (\"me\" = the operator):\n\n%s", c.Name, kind, transcript)
	resp, _, _, err := sess.Chat(ctx, prompt)
	if err != nil {
		s.log.Warn("cold-scan: summarize failed", zap.String("chat", c.Name), zap.Error(err))
		return "", false
	}
	resp = strings.TrimSpace(resp)
	if resp == "" || strings.EqualFold(resp, "SKIP") {
		return "", false
	}
	return resp, true
}
