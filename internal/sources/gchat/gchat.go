// Package gchat turns new Google Chat messages on one account into
// sources.Event: every message in a space or DM that the operator did not
// write, urgent when it is a person writing to them directly or mentioning
// them.
//
// It is pure logic. How spaces and messages are read is the API seam, where
// cursors live is the Store seam, and what was already told is sources.Seen.
//
// It replaces the gchat-watch workflow, which read Chat on a four-hour cron
// for one account and decided what to do inside the loop. Here a message is an
// event like any other, decided on by the same core as everything else.
package gchat

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

// SourceName is the source the Chat events carry.
const SourceName = "gchat"

// Space is one space, group chat or DM.
type Space struct {
	Name        string // spaces/AAAA
	DisplayName string
	Type        string // SPACE, GROUP_CHAT, DIRECT_MESSAGE
	// LastActive is when the space last had a message. Zero when the path
	// cannot say, in which case the space is probed in turn.
	LastActive time.Time
}

// Message is one message.
type Message struct {
	Name       string // spaces/AAAA/messages/BBBB
	Space      string
	Thread     string // spaces/AAAA/threads/CCCC
	Text       string
	Sender     string // users/123, when the path gives it
	SenderName string
	SenderType string // HUMAN or BOT; empty when unknown
	Created    time.Time
	// Mentions are the users the message @-mentions. Nil when the path cannot
	// say, in which case a mention is recognised by the operator's name.
	Mentions []string
}

// Me is the account's own Chat identity.
type Me struct {
	User  string `json:"user,omitempty"` // users/123
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// Person is what is known of a sender.
type Person struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// API reads one account's Chat.
type API interface {
	Me(ctx context.Context, account string) (Me, error)
	Spaces(ctx context.Context, account string) ([]Space, error)
	// Messages returns up to max messages created after `after`, oldest first.
	Messages(ctx context.Context, account, space string, after time.Time, max int) ([]Message, error)
	// LastRead is when the operator last read the space; zero when never.
	LastRead(ctx context.Context, account, space string) (time.Time, error)
	// Person names a user, for a title a person can read.
	Person(ctx context.Context, account, user string) (Person, error)
}

// State is what is remembered about one account between ticks.
type State struct {
	FirstSeen time.Time            `json:"first_seen"`
	Me        Me                   `json:"me"`
	Cursors   map[string]time.Time `json:"cursors"`
	Names     map[string]Person    `json:"names,omitempty"`
	Rotate    int                  `json:"rotate,omitempty"`
}

// Store keeps State per account.
type Store interface {
	Load(account string) (State, bool, error)
	Save(account string, st State) error
}

// Config tunes the watcher. Zero values take the defaults.
type Config struct {
	Location *time.Location
	// FirstLook is how far back an account's first look reads unread messages (24h).
	FirstLook time.Duration
	// MaxSpaces bounds the spaces read in one tick (40).
	MaxSpaces int
	// MaxMessages bounds the messages read from one space in one tick (50).
	MaxMessages int
	// MaxLookups bounds sender-name lookups in one tick (20).
	MaxLookups int
}

func (c Config) withDefaults() Config {
	if c.Location == nil {
		c.Location = time.UTC
	}
	if c.FirstLook <= 0 {
		c.FirstLook = 24 * time.Hour
	}
	if c.MaxSpaces <= 0 {
		c.MaxSpaces = 40
	}
	if c.MaxMessages <= 0 {
		c.MaxMessages = 50
	}
	if c.MaxLookups <= 0 {
		c.MaxLookups = 20
	}
	return c
}

// Watcher watches Chat.
type Watcher struct {
	cfg   Config
	api   API
	store Store
	seen  sources.Seen
	now   func() time.Time
}

// New builds a watcher.
func New(cfg Config, api API, store Store, seen sources.Seen) *Watcher {
	return &Watcher{cfg: cfg.withDefaults(), api: api, store: store, seen: seen, now: time.Now}
}

// Result says what one tick did.
type Result struct {
	First     bool
	Spaces    int      // spaces listed
	Read      int      // spaces whose messages were read
	Published int      // events published
	Failed    []string // spaces that could not be read; tried again next tick
}

// Tick reads one account's new messages and publishes them.
//
// A space that cannot be read keeps its cursor and is tried again; only a
// failure that makes the whole account unreadable fails the tick.
func (w *Watcher) Tick(ctx context.Context, account string, publish func(sources.Event) error) (Result, error) {
	now := w.now()
	st, found, err := w.store.Load(account)
	if err != nil {
		return Result{}, fmt.Errorf("gchat %s: load state: %w", account, err)
	}
	first := !found || st.FirstSeen.IsZero()
	if first {
		st.FirstSeen = now
	}
	if st.Cursors == nil {
		st.Cursors = map[string]time.Time{}
	}
	if st.Names == nil {
		st.Names = map[string]Person{}
	}
	if st.Me.User == "" && st.Me.Name == "" {
		// Without this the operator's own messages cannot be told apart from
		// everybody else's, and would be published as though someone wrote
		// to them.
		m, err := w.api.Me(ctx, account)
		if err != nil {
			return Result{}, fmt.Errorf("gchat %s: who is the operator here: %w", account, err)
		}
		if m.User == "" && m.Name == "" {
			return Result{}, fmt.Errorf("gchat %s: could not tell which Chat user this account is", account)
		}
		if m.Email == "" {
			m.Email = account
		}
		st.Me = m
	}

	spaces, err := w.api.Spaces(ctx, account)
	if err != nil {
		return Result{}, err
	}
	res := Result{First: first, Spaces: len(spaces)}
	horizon := st.FirstSeen.Add(-w.cfg.FirstLook)

	var active, unknown []Space
	for _, sp := range spaces {
		if sp.Name == "" {
			continue
		}
		cur, has := st.Cursors[sp.Name]
		switch {
		case sp.LastActive.IsZero():
			unknown = append(unknown, sp)
		case has && !sp.LastActive.After(cur):
		case !has && !sp.LastActive.After(horizon):
			st.Cursors[sp.Name] = sp.LastActive // quiet since before the first look
		default:
			active = append(active, sp)
		}
	}
	sort.SliceStable(active, func(i, j int) bool { return active[i].LastActive.After(active[j].LastActive) })
	todo := active
	if n := len(unknown); n > 0 {
		start := st.Rotate % n
		take := w.cfg.MaxSpaces - len(todo)
		if take > n {
			take = n
		}
		for i := 0; i < take; i++ {
			todo = append(todo, unknown[(start+i)%n])
		}
		if take > 0 {
			st.Rotate = (start + take) % n
		}
	}
	if len(todo) > w.cfg.MaxSpaces {
		todo = todo[:w.cfg.MaxSpaces] // the rest keep their cursors and come next tick
	}

	lookups := 0
	var out []sources.Event
	for _, sp := range todo {
		cur, has := st.Cursors[sp.Name]
		after := cur
		var lastRead time.Time
		readKnown := false
		if !has {
			after = horizon
			if lr, err := w.api.LastRead(ctx, account, sp.Name); err == nil {
				lastRead, readKnown = lr, true
			}
		}
		msgs, err := w.api.Messages(ctx, account, sp.Name, after, w.cfg.MaxMessages)
		if err != nil {
			res.Failed = append(res.Failed, sp.Name)
			continue
		}
		res.Read++
		newest := after
		for _, m := range msgs {
			if m.Created.After(newest) {
				newest = m.Created
			}
			if w.own(st.Me, m) {
				continue
			}
			if !has {
				// A space seen for the first time: what was said since KARMAX
				// started watching is new, and so is what is still unread
				// from the day before.
				unread := readKnown && m.Created.After(lastRead)
				if !m.Created.After(st.FirstSeen) && !unread {
					continue
				}
			}
			if m.Space == "" {
				m.Space = sp.Name
			}
			p := w.sender(ctx, account, &st, m, &lookups)
			e := w.event(account, st.Me, sp, m, p)
			// Said before KARMAX started watching this account: context for
			// the agent, never a page to the operator, however direct.
			e.Backlog = !m.Created.After(st.FirstSeen)
			out = append(out, e)
		}
		if !has && newest.Before(st.FirstSeen) && len(msgs) == 0 {
			newest = st.FirstSeen
		}
		st.Cursors[sp.Name] = newest
	}

	for _, e := range out {
		isNew, err := w.seen.First(e.Key)
		if err != nil {
			return res, fmt.Errorf("gchat %s: seen: %w", account, err)
		}
		if !isNew {
			continue
		}
		if err := publish(e); err != nil {
			return res, fmt.Errorf("gchat %s: publish: %w", account, err)
		}
		res.Published++
	}
	if err := w.store.Save(account, st); err != nil {
		return res, fmt.Errorf("gchat %s: save state: %w", account, err)
	}
	return res, nil
}

// own reports whether the operator wrote m.
func (w *Watcher) own(me Me, m Message) bool { return me.Wrote(m) }

// Wrote reports whether the account's own Chat user wrote m: by the sender's
// user when both sides have one, and by name only when the path gives no user
// at all.
func (me Me) Wrote(m Message) bool {
	if m.Sender != "" && me.User != "" {
		return m.Sender == me.User
	}
	return m.Sender == "" && me.Name != "" && strings.EqualFold(strings.TrimSpace(m.SenderName), me.Name)
}

// mentionsMe reports whether m @-mentions the operator.
func mentionsMe(me Me, m Message) bool {
	if m.Mentions != nil {
		for _, u := range m.Mentions {
			if u == me.User && u != "" {
				return true
			}
		}
		return false
	}
	return me.Name != "" && strings.Contains(strings.ToLower(m.Text), "@"+strings.ToLower(me.Name))
}

// sender names who wrote m, looking a user up at most once.
func (w *Watcher) sender(ctx context.Context, account string, st *State, m Message, lookups *int) Person {
	p := Person{Name: strings.TrimSpace(m.SenderName)}
	if m.Sender == "" {
		return p
	}
	if known, ok := st.Names[m.Sender]; ok {
		if p.Name == "" {
			p.Name = known.Name
		}
		p.Email = known.Email
		return p
	}
	if *lookups >= w.cfg.MaxLookups {
		return p
	}
	*lookups++
	found, err := w.api.Person(ctx, account, m.Sender)
	if err != nil {
		found = Person{} // remembered as unknown, so it is not asked every tick
	}
	st.Names[m.Sender] = found
	if p.Name == "" {
		p.Name = found.Name
	}
	p.Email = found.Email
	return p
}

func spaceID(name string) string { return strings.TrimPrefix(name, "spaces/") }

func spaceURL(sp Space) string {
	if sp.Type == "DIRECT_MESSAGE" {
		return "https://chat.google.com/dm/" + spaceID(sp.Name)
	}
	return "https://chat.google.com/room/" + spaceID(sp.Name)
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (w *Watcher) event(account string, me Me, sp Space, m Message, p Person) sources.Event {
	from := p.Name
	if from == "" {
		from = p.Email
	}
	if from == "" {
		from = m.Sender
	}
	if from == "" {
		from = "someone"
	}
	where := sp.DisplayName
	if where == "" {
		where = sp.Name
	}
	text := m.Text
	if strings.TrimSpace(text) == "" {
		text = "(no text: an attachment or card)"
	}
	bot := strings.EqualFold(m.SenderType, "BOT")
	dm := sp.Type == "DIRECT_MESSAGE"
	mention := mentionsMe(me, m)

	e := sources.Event{Source: SourceName, Account: account, Key: SourceName + "|" + strings.ToLower(account) + "|" + m.Name,
		URL: spaceURL(sp), At: m.Created}
	switch {
	case dm:
		e.Type = "dm"
		e.Urgent = !bot
		e.Title = fmt.Sprintf("Chat DM from %s: %s", from, snippet(text, 100))
	case mention:
		e.Type = "mention"
		e.Urgent = !bot
		e.Title = fmt.Sprintf("%s mentioned you in %s: %s", from, where, snippet(text, 100))
	default:
		e.Type = "message"
		e.Title = fmt.Sprintf("%s — %s: %s", where, from, snippet(text, 100))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s", from)
	if p.Email != "" && p.Email != from {
		fmt.Fprintf(&b, " <%s>", p.Email)
	}
	if bot {
		b.WriteString(" (an app, not a person)")
	}
	b.WriteString("\n")
	if dm {
		b.WriteString("Where: a direct message to the operator\n")
	} else {
		fmt.Fprintf(&b, "Where: %s (%s)\n", where, strings.ToLower(strings.ReplaceAll(sp.Type, "_", " ")))
	}
	fmt.Fprintf(&b, "Sent: %s\n", m.Created.In(w.cfg.Location).Format("Mon 2 Jan 15:04 MST"))
	if r := []rune(strings.TrimSpace(text)); len(r) > 2000 {
		text = string(r[:2000]) + "…"
	}
	fmt.Fprintf(&b, "Message:\n%s\n", strings.TrimSpace(text))
	fmt.Fprintf(&b, "Chat account: %s (reply as this account: space %s", account, sp.Name)
	if m.Thread != "" {
		fmt.Fprintf(&b, ", thread %s", m.Thread)
	}
	b.WriteString(")")
	e.Summary = b.String()

	e.Data = map[string]any{
		"space":       sp.Name,
		"space_name":  sp.DisplayName,
		"space_type":  sp.Type,
		"message":     m.Name,
		"thread":      m.Thread,
		"sender":      m.Sender,
		"sender_name": p.Name,
		"mentions_me": mention,
		"from_app":    bot,
	}
	if p.Email != "" {
		e.Data["sender_email"] = p.Email
	}
	return e
}

// ErrNotWorkspace is returned by a path when the account is a personal Google
// account, which Google Chat's API does not serve.
var ErrNotWorkspace = errors.New("gchat: Google Chat's API serves Workspace accounts only, not personal gmail.com ones")
