// Package chatmemory keeps long-term memory in step with the operator's chats:
// WhatsApp, Google Chat, any service a Source can read.
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
// A Syncer reads every chat's new messages from one service — everyone's,
// the operator's own included — sets them beside what memory already holds
// about them, and asks a model what in them is a durable fact, and what it
// adds to, corrects or retires. Each chat keeps a cursor, so a message is read
// once; a chat with no cursor starts at Config.Since (or a lookback before
// KARMAX first looked at its account), which is how the same loop backfills
// history. One Syncer runs per service, each with its own cursors and
// settings, over the same code.
package chatmemory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
	"go.uber.org/zap"
)

// Chat is one conversation as its service lists it.
type Chat struct {
	// ID is the service's own id for the conversation: a WhatsApp JID, a
	// Google Chat space.
	ID   string
	Name string
	// IsGroup is a conversation of more than the operator and one other.
	IsGroup bool
	// Kind is what the service calls this conversation — "group", "space",
	// "direct message" — for the prompt. Empty is "group" or "direct chat".
	Kind string
	// Account is the operator's login the chat is read through, for a service
	// with several; "" for a service with one. The same ID on two accounts is
	// two conversations.
	Account       string
	LastMessageAt time.Time
}

// Message is one message.
type Message struct {
	ID     string
	Chat   string
	Sender string
	// SenderName is the name the message itself carries for its sender; ""
	// leaves the name to Source.Name.
	SenderName string
	Content    string
	At         time.Time
	// FromMe is a message sent from the operator's own account: typed by the
	// operator, or sent by KARMAX, which speaks through the same account.
	FromMe bool
}

// Source reads one chat service's history.
type Source interface {
	// Chats lists the conversations the operator is in. Channels broadcast at
	// the operator are not conversations and are not listed.
	Chats(ctx context.Context) ([]Chat, error)
	// Messages returns up to limit of a chat's messages that the cursor has
	// not passed (see Cursor), oldest first. It answers with none only when
	// none follows the cursor: a source that reads in pages or windows looks
	// past one that holds only messages the cursor has passed.
	Messages(ctx context.Context, chat Chat, after Cursor, limit int) ([]Message, error)
	// Name is a sender's display name, or "" when the service knows none.
	Name(ctx context.Context, sender string) string
}

// Memory is the part of the memory manager the syncer uses.
type Memory interface {
	SearchWith(query string, topK int, opts memory.SearchOpts) ([]memory.SearchResult, error)
	Write(entry memory.MemoryEntry) error
	// ForgetFact removes one fact, and refuses (false, nil) a handle naming a
	// document of several.
	ForgetFact(ctx context.Context, handle string) (bool, error)
}

// Cursor is how far into a chat the syncer has read: through the message ID
// at time At or, with no ID, up to the moment At and past none of the
// messages of that time.
//
// A chat's messages are read in one order: by time, and messages of the same
// time by ID. WhatsApp times a message to the second, so a burst of messages
// shares one time; ordering them by ID as well is what lets a cursor stop
// between two of them and resume after the right one, instead of taking the
// others of its second for new every time it is read from.
type Cursor struct {
	At time.Time
	ID string
}

// before reports whether c comes before d in a chat's order.
func (c Cursor) before(d Cursor) bool {
	if !c.At.Equal(d.At) {
		return c.At.Before(d.At)
	}
	return c.ID < d.ID
}

// Passed reports whether the cursor has passed m, which comes at or before it
// in the chat's order and so has been read.
func (c Cursor) Passed(m Message) bool { return !c.before(cursorOn(m)) }

// cursorOn is the cursor standing on m.
func cursorOn(m Message) Cursor { return Cursor{At: m.At, ID: m.ID} }

// unread is what of ms the cursor has not passed, in the chat's order.
func unread(ms []Message, c Cursor) []Message {
	var out []Message
	for _, m := range ms {
		if !c.Passed(m) {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return cursorOn(out[i]).before(cursorOn(out[j])) })
	return out
}

// Cursors persists each chat's cursor.
type Cursors interface {
	Get(chat string) (Cursor, bool, error)
	Set(chat string, c Cursor) error
}

// Config tunes a Syncer. The zero value of each field has a working default.
type Config struct {
	// Name is the syncer's own name, for logs and errors: "wa-memory".
	Name string
	// Service is the chat service as a person names it — "WhatsApp", "Google
	// Chat" — which the prompt says where it matters.
	Service string
	// Tag files every fact under its service as well as its subject:
	// "whatsapp", "gchat".
	Tag string
	// Operator is the name the operator's own messages are attributed to.
	Operator string
	// OperatorIDs are the operator's other accounts; their messages are his.
	OperatorIDs []string
	// IsOperator, when set, is the identity directory's answer to "is this
	// account the operator's" — which also knows the LIDs it has learned since
	// the configuration was written. OperatorIDs still count either way.
	IsOperator func(id string) bool
	// Since, when set, is where a chat with no cursor starts reading.
	Since time.Time
	// Lookback is, when Since is not set, how long before KARMAX first looked
	// at a chat's account the chat starts reading (a week).
	Lookback time.Duration
	// FirstLook is when KARMAX first looked at an account, recorded on the
	// first ask so a restart does not move it. Nil is when the Syncer was made.
	FirstLook func(account string) (time.Time, error)
	// Deny names chats never read, by ID or by name.
	Deny []string
	// BatchesPerTick bounds the model calls one tick may make.
	BatchesPerTick int
	// BatchChars and BatchMessages bound one batch.
	BatchChars    int
	BatchMessages int
	// Location is the operator's zone: messages are shown, and facts dated,
	// by the operator's own day. Nil is the machine's zone.
	Location *time.Location
}

// Result is what one tick did.
type Result struct {
	Batches, Added, Updated, Forgotten, Refused int
	// More is work left for the next tick.
	More bool
	// Stalled are the chats, by cursor key, whose cursor could not move
	// although they have newer messages: their source answered with only
	// messages already read. Each is logged at warn, left where it was, and
	// tried again next tick.
	Stalled []string
}

// Syncer turns one service's new messages into memory.
type Syncer struct {
	cfg  Config
	src  Source
	mem  Memory
	cur  Cursors
	sent func(c Chat, text string, at time.Time) bool
	ask  func(ctx context.Context, prompt string) (string, error)
	log  *zap.Logger
	made time.Time

	operators map[string]bool
	deny      map[string]bool
}

// New builds a Syncer. sent reports whether KARMAX itself sent a message, which
// is the only way to tell its words from the operator's on the same account.
func New(cfg Config, src Source, mem Memory, cur Cursors, sent func(c Chat, text string, at time.Time) bool,
	ask func(ctx context.Context, prompt string) (string, error), log *zap.Logger) *Syncer {
	if cfg.Name == "" {
		cfg.Name = "chat-memory"
	}
	if cfg.Tag == "" {
		cfg.Tag = "chat"
	}
	if cfg.Operator == "" {
		cfg.Operator = "the operator"
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = 7 * 24 * time.Hour
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
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if sent == nil {
		sent = func(Chat, string, time.Time) bool { return false }
	}
	if log == nil {
		log = zap.NewNop()
	}
	s := &Syncer{cfg: cfg, src: src, mem: mem, cur: cur, sent: sent, ask: ask, log: log, made: time.Now(),
		operators: map[string]bool{}, deny: map[string]bool{}}
	for _, j := range cfg.OperatorIDs {
		s.operators[bare(j)] = true
	}
	for _, d := range cfg.Deny {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			s.deny[d] = true
		}
	}
	return s
}

// bare is an id without its device or server: WhatsApp's
// "91999:12@s.whatsapp.net" and "91999@s.whatsapp.net" are one person. An id
// with neither ("users/123") is itself.
func bare(id string) string {
	user, _, _ := strings.Cut(id, "@")
	user, _, _ = strings.Cut(user, ":")
	return user
}

// shownID is how a sender nobody can name is written: a phone number as one,
// any other id as itself.
func shownID(key string) string {
	if key == "" {
		return "someone"
	}
	for _, r := range key {
		if r < '0' || r > '9' {
			return key
		}
	}
	return "+" + key
}

// key is a chat's cursor key: its ID, qualified by its account when the
// service has several. A chat on a single-account service keeps the bare ID,
// which is how its cursors were always kept.
func key(c Chat) string {
	if c.Account == "" {
		return c.ID
	}
	return c.Account + "|" + c.ID
}

func (s *Syncer) skip(c Chat) bool {
	return s.deny[strings.ToLower(c.ID)] || (c.Name != "" && s.deny[strings.ToLower(c.Name)])
}

// start is where a chat with no cursor begins: Since when it is set, else a
// lookback before KARMAX first looked at the chat's account.
func (s *Syncer) start(c Chat, looked map[string]time.Time) (time.Time, error) {
	if !s.cfg.Since.IsZero() {
		return s.cfg.Since, nil
	}
	at, ok := looked[c.Account]
	if !ok {
		at = s.made
		if s.cfg.FirstLook != nil {
			var err error
			if at, err = s.cfg.FirstLook(c.Account); err != nil {
				return time.Time{}, err
			}
		}
		looked[c.Account] = at
	}
	return at.Add(-s.cfg.Lookback), nil
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
		return res, fmt.Errorf("%s: list chats: %w", s.cfg.Name, err)
	}
	looked := map[string]time.Time{}
	var queue []pending
	for _, c := range chats {
		if s.skip(c) {
			continue
		}
		cur, ok, err := s.cur.Get(key(c))
		if err != nil {
			return res, fmt.Errorf("%s: cursor for %s: %w", s.cfg.Name, key(c), err)
		}
		if !ok {
			at, err := s.start(c, looked)
			if err != nil {
				return res, fmt.Errorf("%s: first look at %s: %w", s.cfg.Name, c.Account, err)
			}
			cur = Cursor{At: at}
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
// whether the chat is done for this tick. The cursor moves only once the batch
// is applied: a pass the model fails is read again next time. It only ever
// moves forward, in the chat's order (see Cursor), so each message is read
// once.
func (s *Syncer) batch(ctx context.Context, p pending, names map[string]string, res *Result) (Cursor, bool, error) {
	raw, err := s.src.Messages(ctx, p.chat, p.cur, s.cfg.BatchMessages+1)
	if err != nil {
		return p.cur, false, fmt.Errorf("%s: messages of %s: %w", s.cfg.Name, key(p.chat), err)
	}
	// Whatever a source hands back that the cursor has passed is dropped.
	fresh := unread(raw, p.cur)

	var (
		next  Cursor
		take  []Message
		lines []string
		peer  string // the other person in a direct chat, for a chat with no name
	)
	switch {
	case len(fresh) > 0:
		size := 0
		for _, m := range fresh {
			line, who, operator := s.line(ctx, p.chat, m, names)
			if len(take) > 0 && size+len(line) > s.cfg.BatchChars {
				break
			}
			take = append(take, m)
			if line != "" {
				lines = append(lines, line)
				size += len(line)
				if peer == "" && !operator {
					peer = who
				}
			}
		}
		next = cursorOn(take[len(take)-1])
	case len(raw) == 0 && p.chat.LastMessageAt.After(p.cur.At):
		// Nothing follows the cursor: the chat's last activity was not a
		// message — a reaction, someone joining. Nothing is there to read up
		// to that activity, so the cursor moves to it: otherwise the chat
		// looks active, and is read again, every tick.
		next = Cursor{At: p.chat.LastMessageAt}
	default:
		// Nothing follows the cursor and nothing happened since; or the
		// source answered with only messages already read, which says
		// nothing of what follows them — moving to the chat's last activity
		// would skip that.
		next = p.cur
	}

	if !p.cur.before(next) {
		if p.chat.LastMessageAt.After(p.cur.At) {
			// Saving the cursor where it was would read the chat again, and
			// stop at the same place, every tick: a stall, said out loud.
			res.Stalled = append(res.Stalled, key(p.chat))
			s.log.Warn(s.cfg.Name+": a chat's cursor cannot move although the chat has newer messages; it is left where it is",
				zap.String("chat", key(p.chat)), zap.Time("cursor_at", p.cur.At), zap.String("cursor_id", p.cur.ID),
				zap.Time("last_message_at", p.chat.LastMessageAt), zap.Int("returned", len(raw)))
		}
		return p.cur, true, nil
	}

	if len(lines) > 0 {
		res.Batches++
		chat := p.chat
		if chat.Name == "" && !chat.IsGroup {
			chat.Name = peer
		}
		if err := s.extract(ctx, chat, lines, next.At, res); err != nil {
			return p.cur, false, err
		}
	}
	if err := s.cur.Set(key(p.chat), next); err != nil {
		return p.cur, false, fmt.Errorf("%s: save cursor for %s: %w", s.cfg.Name, key(p.chat), err)
	}
	done := len(take) == len(fresh) && len(raw) <= s.cfg.BatchMessages
	return next, done, nil
}

// line renders one message for the model, naming who said it and whether that
// was the operator (or KARMAX on their account); "" for a message with
// nothing to read (a sticker, a photo without a caption).
func (s *Syncer) line(ctx context.Context, chat Chat, m Message, names map[string]string) (line, who string, operator bool) {
	text := strings.Join(strings.Fields(m.Content), " ")
	if text == "" {
		return "", "", false
	}
	if len(text) > s.cfg.BatchChars {
		text = text[:s.cfg.BatchChars] + "…"
	}
	who, operator = s.speaker(ctx, chat, m, names)
	return m.At.In(s.cfg.Location).Format("2006-01-02 15:04") + " " + who + ": " + text, who, operator
}

// speaker names who wrote m, decided by the account it came from and never
// by a label: the operator's own account is the operator, whatever it is
// called.
func (s *Syncer) speaker(ctx context.Context, chat Chat, m Message, names map[string]string) (string, bool) {
	switch {
	case m.FromMe && s.sent(chat, m.Content, m.At):
		return "KARMAX (on " + s.cfg.Operator + "'s account)", true
	case m.FromMe, s.operators[bare(m.Sender)]:
		return s.cfg.Operator, true
	case s.cfg.IsOperator != nil && s.cfg.IsOperator(m.Sender):
		return s.cfg.Operator, true
	}
	if n := strings.TrimSpace(m.SenderName); n != "" {
		return n, false
	}
	k := bare(m.Sender)
	if n, ok := names[k]; ok {
		return n, false
	}
	n := strings.TrimSpace(s.src.Name(ctx, m.Sender))
	if n == "" && !chat.IsGroup {
		n = chat.Name
	}
	if n == "" {
		n = shownID(k)
	}
	names[k] = n
	return n, false
}
