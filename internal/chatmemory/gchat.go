package chatmemory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/sources/gchat"
	"go.uber.org/zap"
)

// GChat reads Google Chat for a Syncer: the spaces, group chats and direct
// messages of every account it is given, and every message in them — the
// operator's own included.
//
// It reads through gchat.API, which the shared Google account layer
// (googleacct) implements over the CLIs signed in on the host, so an account
// is reached the same way, with the same fallback between logins, as the
// Chat event source reaches it.
//
// The operator is told apart by the account's own Chat user (gchat.Me),
// never by a display name: a message from that user is FromMe, and the
// syncer names it as the operator. An account whose own user cannot be told
// is not read at all, since its operator's words would be taken for someone
// else's.
type GChat struct {
	api      gchat.API
	accounts func(ctx context.Context) []string
	log      *zap.Logger
	now      func() time.Time

	mu    sync.Mutex
	me    map[string]gchat.Me
	names map[string]string // user -> display name; "" when none could be found
	via   map[string]string // user -> an account that saw them, to look them up through
}

// NewGChat reads the Chat of the accounts that accounts lists, asked afresh
// each tick so an account signed in later is read without a restart.
func NewGChat(api gchat.API, accounts func(ctx context.Context) []string, log *zap.Logger) *GChat {
	if log == nil {
		log = zap.NewNop()
	}
	return &GChat{api: api, accounts: accounts, log: log, now: time.Now,
		me: map[string]gchat.Me{}, names: map[string]string{}, via: map[string]string{}}
}

// who is the account's own Chat user, asked once.
func (g *GChat) who(ctx context.Context, account string) (gchat.Me, error) {
	g.mu.Lock()
	me, ok := g.me[account]
	g.mu.Unlock()
	if ok {
		return me, nil
	}
	me, err := g.api.Me(ctx, account)
	if err != nil {
		return gchat.Me{}, err
	}
	if me.User == "" && me.Name == "" {
		return gchat.Me{}, errors.New("could not tell which Chat user this account is")
	}
	g.mu.Lock()
	g.me[account] = me
	g.mu.Unlock()
	return me, nil
}

// kind is what Google Chat calls a space, as the prompt says it.
func kind(spaceType string) string {
	switch spaceType {
	case "":
		return ""
	case "DIRECT_MESSAGE":
		return "direct message"
	case "GROUP_CHAT":
		return "group chat"
	case "SPACE":
		return "space"
	}
	return strings.ToLower(strings.ReplaceAll(spaceType, "_", " "))
}

// Chats lists every account's spaces, with when each was last active, so the
// syncer reads only the ones with something new. An account that cannot be
// read is left out of this tick; only when none can is it an error.
func (g *GChat) Chats(ctx context.Context) ([]Chat, error) {
	var (
		out    []Chat
		failed []string
		read   bool
	)
	for _, account := range g.accounts(ctx) {
		if _, err := g.who(ctx, account); err != nil {
			g.log.Warn("gchat-memory: not reading an account whose own Chat user is unknown",
				zap.String("account", account), zap.Error(err))
			failed = append(failed, account+": "+err.Error())
			continue
		}
		spaces, err := g.api.Spaces(ctx, account)
		if err != nil {
			g.log.Warn("gchat-memory: could not list an account's spaces", zap.String("account", account), zap.Error(err))
			failed = append(failed, account+": "+err.Error())
			continue
		}
		read = true
		for _, sp := range spaces {
			if sp.Name == "" {
				continue
			}
			last := sp.LastActive
			if last.IsZero() {
				// The path cannot say when it was last active, so it is read
				// in turn rather than never.
				last = g.now()
			}
			out = append(out, Chat{ID: sp.Name, Account: account, Name: strings.TrimSpace(sp.DisplayName),
				IsGroup: sp.Type != "DIRECT_MESSAGE", Kind: kind(sp.Type), LastMessageAt: last})
		}
	}
	if !read && len(failed) > 0 {
		return nil, errors.New(strings.Join(failed, "; "))
	}
	return out, nil
}

// Messages returns up to limit of a space's messages that the cursor has not
// passed, oldest first, from every sender. A message from an app says so in
// its sender's name.
//
// Chat is asked for what follows a second before the cursor, so the messages
// of the cursor's own time are there whichever way the API's bound falls; the
// ones the cursor has passed are dropped. A full page of only those says
// nothing of what follows it, and is handed back as it is: the syncer then
// reports the space stalled, where an empty answer would read as its end.
func (g *GChat) Messages(ctx context.Context, c Chat, after Cursor, limit int) ([]Message, error) {
	me, err := g.who(ctx, c.Account)
	if err != nil {
		return nil, err
	}
	msgs, err := g.api.Messages(ctx, c.Account, c.ID, after.At.Add(-time.Second), limit)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(msgs))
	g.mu.Lock()
	for _, m := range msgs {
		name := strings.TrimSpace(m.SenderName)
		if strings.EqualFold(m.SenderType, "BOT") {
			if name == "" {
				name = "an app"
			} else {
				name += " (an app)"
			}
		}
		if m.Sender != "" {
			if _, ok := g.via[m.Sender]; !ok {
				g.via[m.Sender] = c.Account
			}
			// The name a message carries is the best there is: the People
			// API often knows less about a colleague than Chat shows.
			if name != "" {
				g.names[m.Sender] = name
			}
		}
		out = append(out, Message{ID: m.Name, Chat: c.ID, Sender: m.Sender, SenderName: name,
			Content: m.Text, At: m.Created, FromMe: me.Wrote(m)})
	}
	g.mu.Unlock()
	fresh := unread(out, after)
	if len(fresh) == 0 && limit > 0 && len(msgs) >= limit {
		return out, nil
	}
	return fresh, nil
}

// Name is a sender's display name (their address when they have none),
// looked up once through an account that saw them; "" when nobody can say.
func (g *GChat) Name(ctx context.Context, sender string) string {
	g.mu.Lock()
	n, known := g.names[sender]
	account := g.via[sender]
	g.mu.Unlock()
	if known || account == "" {
		return n
	}
	if p, err := g.api.Person(ctx, account, sender); err == nil {
		n = strings.TrimSpace(p.Name)
		if n == "" {
			n = strings.TrimSpace(p.Email)
		}
	}
	// Remembered either way, so an unknown sender is not asked about every tick.
	g.mu.Lock()
	g.names[sender] = n
	g.mu.Unlock()
	return n
}
