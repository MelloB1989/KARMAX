// Package wamemory keeps long-term memory in step with WhatsApp.
//
// Memory used to learn from WhatsApp only by accident. The agent saw the
// operator's own chats and remembered what it happened to think worth
// remembering; the monitoring loops saw a few chosen chats and filed their own
// outcome lines — "INFORM: …", "[flagged] no new messages" — into two ever
// growing files. A deadline someone moved in a group, a person who changed
// jobs, a plan that was cancelled: none of it reached memory unless KARMAX was
// asked about it at the time, and nothing that memory already held was ever
// corrected by what was said afterwards.
//
// The syncer reads every chat's new messages from wacli, sets them beside what
// memory already holds about them, and asks a model what in them is a durable
// fact, and what it adds to, corrects or retires. Each chat keeps a cursor, so
// a message is read once; a chat with no cursor starts at Config.Since, which
// is how the same loop backfills history.
package wamemory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
	"go.uber.org/zap"
)

// Chat is one WhatsApp conversation as wacli lists it.
type Chat struct {
	JID           string
	Name          string
	IsGroup       bool
	LastMessageAt time.Time
}

// Message is one WhatsApp message as wacli stores it.
type Message struct {
	ID        string
	ChatJID   string
	SenderJID string
	Content   string
	At        time.Time
	// FromMe is a message sent from the linked account: typed by the operator
	// on that phone, or sent by KARMAX, which speaks through the same account.
	FromMe bool
}

// Source reads WhatsApp history.
type Source interface {
	Chats(ctx context.Context) ([]Chat, error)
	// Messages returns a chat's messages after a time, oldest first.
	Messages(ctx context.Context, chatJID string, after time.Time, limit int) ([]Message, error)
	// Name is a sender's display name, or "" when wacli knows none.
	Name(ctx context.Context, jid string) string
}

// Memory is the part of the memory manager the syncer uses.
type Memory interface {
	SearchWith(query string, topK int, opts memory.SearchOpts) ([]memory.SearchResult, error)
	Write(entry memory.MemoryEntry) error
	// ForgetFact removes one fact, and refuses (false, nil) a handle naming a
	// document of several.
	ForgetFact(ctx context.Context, handle string) (bool, error)
}

// Cursor is how far into a chat the syncer has read.
type Cursor struct {
	At time.Time
	ID string
}

// Cursors persists each chat's cursor.
type Cursors interface {
	Get(chat string) (Cursor, bool, error)
	Set(chat string, c Cursor) error
}

// Config tunes a Syncer. The zero value of each field has a working default.
type Config struct {
	// Operator is the name the operator's own messages are attributed to.
	Operator string
	// OperatorJIDs are the operator's other accounts; their messages are his.
	OperatorJIDs []string
	// Since is where a chat with no cursor starts reading.
	Since time.Time
	// Deny names chats never read, by JID or by name.
	Deny []string
	// BatchesPerTick bounds the model calls one tick may make.
	BatchesPerTick int
	// BatchChars and BatchMessages bound one batch.
	BatchChars    int
	BatchMessages int
}

// Result is what one tick did.
type Result struct {
	Batches, Added, Updated, Forgotten, Refused int
	// More is work left for the next tick.
	More bool
}

// Syncer turns new WhatsApp messages into memory.
type Syncer struct {
	cfg  Config
	src  Source
	mem  Memory
	cur  Cursors
	sent func(chat, text string, at time.Time) bool
	ask  func(ctx context.Context, prompt string) (string, error)
	log  *zap.Logger

	operators map[string]bool
	deny      map[string]bool
}

// New builds a Syncer. sent reports whether KARMAX itself sent a message, which
// is the only way to tell its words from the operator's on the same account.
func New(cfg Config, src Source, mem Memory, cur Cursors, sent func(chat, text string, at time.Time) bool,
	ask func(ctx context.Context, prompt string) (string, error), log *zap.Logger) *Syncer {
	if cfg.Operator == "" {
		cfg.Operator = "the operator"
	}
	if cfg.BatchesPerTick <= 0 {
		cfg.BatchesPerTick = 12
	}
	if cfg.BatchChars <= 0 {
		cfg.BatchChars = 8000
	}
	if cfg.BatchMessages <= 0 {
		cfg.BatchMessages = 150
	}
	if sent == nil {
		sent = func(string, string, time.Time) bool { return false }
	}
	s := &Syncer{cfg: cfg, src: src, mem: mem, cur: cur, sent: sent, ask: ask, log: log,
		operators: map[string]bool{}, deny: map[string]bool{}}
	for _, j := range cfg.OperatorJIDs {
		s.operators[bare(j)] = true
	}
	for _, d := range cfg.Deny {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			s.deny[d] = true
		}
	}
	return s
}

// bare is a JID's account, without its device or server: "91999:12@s.whatsapp.net"
// and "91999@s.whatsapp.net" are one person.
func bare(jid string) string {
	user, _, _ := strings.Cut(jid, "@")
	user, _, _ = strings.Cut(user, ":")
	return user
}

func (s *Syncer) skip(c Chat) bool {
	if strings.HasSuffix(c.JID, "@newsletter") || strings.HasSuffix(c.JID, "@broadcast") {
		return true // channels and status updates: broadcast at the operator, not said to him
	}
	return s.deny[strings.ToLower(c.JID)] || (c.Name != "" && s.deny[strings.ToLower(c.Name)])
}

type pending struct {
	chat Chat
	cur  Cursor
}

// Tick reads what is new, chat by chat, and applies it to memory.
//
// The most recently active chats go first and each gets one batch per pass,
// so a backfill of a long history never holds up what was said today.
func (s *Syncer) Tick(ctx context.Context) (Result, error) {
	var res Result
	chats, err := s.src.Chats(ctx)
	if err != nil {
		return res, fmt.Errorf("wa-memory: list chats: %w", err)
	}
	var queue []pending
	for _, c := range chats {
		if s.skip(c) {
			continue
		}
		cur, ok, err := s.cur.Get(c.JID)
		if err != nil {
			return res, fmt.Errorf("wa-memory: cursor for %s: %w", c.JID, err)
		}
		if !ok {
			cur = Cursor{At: s.cfg.Since}
		}
		if c.LastMessageAt.After(cur.At) {
			queue = append(queue, pending{chat: c, cur: cur})
		}
	}
	sort.SliceStable(queue, func(i, j int) bool { return queue[i].chat.LastMessageAt.After(queue[j].chat.LastMessageAt) })

	names := map[string]string{}
	for len(queue) > 0 {
		var next []pending
		for _, p := range queue {
			if res.Batches >= s.cfg.BatchesPerTick {
				res.More = true
				return res, nil
			}
			cur, done, err := s.batch(ctx, p, names, &res)
			if err != nil {
				res.More = true
				return res, err
			}
			if !done {
				next = append(next, pending{chat: p.chat, cur: cur})
			}
		}
		queue = next
	}
	return res, nil
}

// batch reads one batch of a chat and applies it, returning the new cursor and
// whether the chat is caught up. The cursor moves only once the batch is
// applied: a pass the model fails is read again next time.
func (s *Syncer) batch(ctx context.Context, p pending, names map[string]string, res *Result) (Cursor, bool, error) {
	// From a second early, because a store's "after" may or may not include
	// its bound; the cursor's own message is dropped below.
	raw, err := s.src.Messages(ctx, p.chat.JID, p.cur.At.Add(-time.Second), s.cfg.BatchMessages+1)
	if err != nil {
		return p.cur, false, fmt.Errorf("wa-memory: messages of %s: %w", p.chat.JID, err)
	}
	var fresh []Message
	for _, m := range raw {
		if m.At.Before(p.cur.At) || (m.At.Equal(p.cur.At) && (m.ID == p.cur.ID || p.cur.ID == "")) {
			continue
		}
		fresh = append(fresh, m)
	}
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].At.Before(fresh[j].At) })
	if len(fresh) == 0 {
		return p.cur, true, nil
	}

	var (
		take  []Message
		lines []string
		size  int
	)
	for _, m := range fresh {
		line := s.line(ctx, p.chat, m, names)
		if len(take) > 0 && size+len(line) > s.cfg.BatchChars {
			break
		}
		take = append(take, m)
		if line != "" {
			lines = append(lines, line)
			size += len(line)
		}
	}
	last := take[len(take)-1]
	cur := Cursor{At: last.At, ID: last.ID}
	done := len(take) == len(fresh) && len(raw) <= s.cfg.BatchMessages

	if len(lines) > 0 {
		res.Batches++
		if err := s.extract(ctx, p.chat, lines, last.At, res); err != nil {
			return p.cur, false, err
		}
	}
	if err := s.cur.Set(p.chat.JID, cur); err != nil {
		return p.cur, false, fmt.Errorf("wa-memory: save cursor for %s: %w", p.chat.JID, err)
	}
	return cur, done, nil
}

// line renders one message for the model, naming who said it; "" for a
// message with nothing to read (a sticker, a photo without a caption).
func (s *Syncer) line(ctx context.Context, chat Chat, m Message, names map[string]string) string {
	text := strings.Join(strings.Fields(m.Content), " ")
	if text == "" {
		return ""
	}
	if len(text) > s.cfg.BatchChars {
		text = text[:s.cfg.BatchChars] + "…"
	}
	return m.At.UTC().Format("2006-01-02 15:04") + " " + s.speaker(ctx, chat, m, names) + ": " + text
}

func (s *Syncer) speaker(ctx context.Context, chat Chat, m Message, names map[string]string) string {
	switch {
	case m.FromMe && s.sent(chat.JID, m.Content, m.At):
		return "KARMAX (on " + s.cfg.Operator + "'s account)"
	case m.FromMe, s.operators[bare(m.SenderJID)]:
		return s.cfg.Operator
	}
	key := bare(m.SenderJID)
	if n, ok := names[key]; ok {
		return n
	}
	n := strings.TrimSpace(s.src.Name(ctx, m.SenderJID))
	if n == "" && !chat.IsGroup {
		n = chat.Name
	}
	if n == "" {
		n = "+" + key
	}
	names[key] = n
	return n
}
