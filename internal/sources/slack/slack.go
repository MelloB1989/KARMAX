// Package slack is the Slack event source: what reaches the operator in every
// workspace they are signed into — direct messages, mentions, and the channels
// they sit in — read with their own user token and published as
// sources.Event. It also sends as the operator, which is how KARMAX answers.
//
// Polled, not pushed. A Slack app's socket belongs to the slack-main comms
// channel (the bot's own conversations), and Slack hands each event to only
// one of an app's sockets, so a second listener here would take events away
// from the bot. A user token reads the operator's conversations without any
// socket, and a cursor per conversation in the store makes a restart resume
// where it stopped.
//
// Reading every conversation every tick would spend Slack's rate limit on
// channels nobody has spoken in for months, so each conversation is read on
// its own schedule: every tick while it is active, every DormantEvery once it
// is quiet, a budget of calls a tick, direct messages first. Mentions inside
// threads never show in a channel's history, so a search for the operator's
// mention catches those.
package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

// Name is the event source's name and route prefix.
const Name = "slack"

// Workspace is one Slack login the source reads as the operator.
type Workspace struct {
	// Integration is the KARMAX integration the tokens came from
	// ("slack-main"), which is what `karmax login` is run with.
	Integration string
	// UserToken (xoxp-…) reads the operator's conversations and sends as them.
	UserToken string
	// BotToken (xoxb-…), when the workspace also has KARMAX's bot, is used
	// only to recognise the bot's messages so KARMAX never hears itself.
	BotToken string
}

// Config tunes the source. Zero values take the defaults below.
type Config struct {
	// Lookback is how far back a conversation never read before is read
	// (default 2h), so connecting a workspace does not replay it as news.
	Lookback time.Duration
	// ActiveWindow: a conversation with a message this recent is read every
	// tick (default 24h).
	ActiveWindow time.Duration
	// DormantEvery is how often a quieter conversation is read (default 15m).
	DormantEvery time.Duration
	// RefreshEvery is how often the list of conversations is fetched again
	// (default 30m).
	RefreshEvery time.Duration
	// CallsPerTick bounds the history reads one tick makes per workspace
	// (default 40; Slack allows about 50 a minute).
	CallsPerTick int
	// MentionQuery is the search that finds the operator's mentions inside
	// threads; {user} is their Slack id. Default "<@{user}>"; "off" disables.
	MentionQuery string
	// Deny names conversations never read, by id or channel name.
	Deny []string
}

func (c Config) withDefaults() Config {
	if c.Lookback <= 0 {
		c.Lookback = 2 * time.Hour
	}
	if c.ActiveWindow <= 0 {
		c.ActiveWindow = 24 * time.Hour
	}
	if c.DormantEvery <= 0 {
		c.DormantEvery = 15 * time.Minute
	}
	if c.RefreshEvery <= 0 {
		c.RefreshEvery = 30 * time.Minute
	}
	if c.CallsPerTick <= 0 {
		c.CallsPerTick = 40
	}
	if strings.TrimSpace(c.MentionQuery) == "" {
		c.MentionQuery = "<@{user}>"
	}
	return c
}

// KV is the store's key-value table (store.Store satisfies it).
type KV interface {
	KVGet(group, key string) (string, bool, error)
	KVSet(group, key, value string, ttl time.Duration) error
}

const (
	cursorGroup    = "slack-source-cursor"
	seenGroup      = "slack-source-seen"
	firstLookGroup = "slack-source-first-look"
	seenTTL        = 3 * 24 * time.Hour // cursors keep reads from overlapping; this covers search and restarts
)

// Source polls Slack workspaces for what concerns the operator.
type Source struct {
	API  string // Web API base, https://slack.com/api
	HTTP *http.Client
	Now  func() time.Time

	cfg       Config
	kv        KV
	seen      sources.Seen
	firstLook sources.FirstLook
	publish   func(sources.Event) error

	tickMu sync.Mutex // one tick at a time
	mu     sync.Mutex // guards teams and what Status and Send read
	teams  map[string]*team
}

// New builds the source. publish delivers each new event once.
func New(cfg Config, kv KV, publish func(sources.Event) error) *Source {
	return &Source{
		API:       "https://slack.com/api",
		HTTP:      &http.Client{Timeout: 40 * time.Second},
		Now:       time.Now,
		cfg:       cfg.withDefaults(),
		kv:        kv,
		seen:      sources.Seen{Group: seenGroup, TTL: seenTTL, Get: kv.KVGet, Set: kv.KVSet},
		firstLook: sources.FirstLook{Group: firstLookGroup, Get: kv.KVGet, Set: kv.KVSet},
		publish:   publish,
		teams:     map[string]*team{},
	}
}

// team is what the source knows about one workspace login.
type team struct {
	integration string
	token, bot  string

	id, name, base, account string
	self                    string
	botUser, botID          string

	convs    map[string]*conv
	listedAt time.Time
	names    map[string]string

	// watchedSince is KARMAX's first look at this workspace: what a
	// conversation's first read finds from before it is backlog.
	watchedSince time.Time

	pausedUntil time.Time
	searchOff   string
	botNote     string
	lastOK      time.Time
	lastErr     string
}

type conv struct {
	id, name, kind string // kind: dm, group_dm, channel
	other          string // a DM's other person
	nextDue        time.Time
	lastActive     time.Time
}

// Result is what one tick did.
type Result struct {
	Workspaces int
	Read       int // conversations read
	Published  int
	// Off, when no workspace could be read, says why and what to run.
	Off string
}

// Status is one workspace's state, for health and logs.
type Status struct {
	Integration   string
	Account       string
	Team          string
	Conversations int
	LastOK        time.Time
	LastError     string
	PausedUntil   time.Time
	// Note is a partial gap, such as search not being allowed.
	Note string
}

// Scopes the user token needs: reading each kind of conversation, names,
// the mention search, and writing as the operator.
const Scopes = "channels:history, groups:history, im:history, mpim:history, channels:read, groups:read, " +
	"im:read, mpim:read, users:read, search:read, chat:write"

// OffReason says what to run so a workspace's events turn on.
func OffReason(integration, problem string) string {
	login := "karmax login " + integration
	if integration == "" {
		login = "add a channel {id: slack-main, type: slack} under comms.channels in karmax.yaml, then karmax login slack-main"
	}
	return fmt.Sprintf("Slack events are off: %s. To turn them on: at https://api.slack.com/apps open the KARMAX app "+
		"(or create one), add the User Token Scopes %s, reinstall it to the workspace, copy the User OAuth Token "+
		"(xoxp-…), then run `%s` and paste it as user_token (or set SLACK_USER_TOKEN). The next poll picks it up; "+
		"no restart is needed.", problem, Scopes, login)
}

// usable separates the workspaces that can be read from the reasons the
// others cannot.
func usable(wss []Workspace) (ok []Workspace, off []string) {
	for _, w := range wss {
		tok := strings.TrimSpace(w.UserToken)
		label := w.Integration
		if label == "" {
			label = "Slack"
		}
		switch {
		case tok == "":
			off = append(off, OffReason(w.Integration, "no user token (xoxp-…) for "+label))
		case !strings.Contains(tok, "xoxp-"):
			off = append(off, OffReason(w.Integration, "the user token for "+label+" is not a user token (it should start xoxp-)"))
		default:
			w.UserToken = tok
			ok = append(ok, w)
		}
	}
	if len(wss) == 0 {
		off = append(off, OffReason("", "no Slack workspace is configured"))
	}
	return ok, off
}

// Tick reads every workspace once.
func (s *Source) Tick(ctx context.Context, wss []Workspace) (Result, error) {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()

	var res Result
	good, off := usable(wss)
	if len(good) == 0 {
		res.Off = strings.Join(off, "\n")
		return res, nil
	}
	var errs []string
	done := map[string]bool{}
	for _, w := range good {
		t := s.team(w)
		now := s.Now()
		if now.Before(t.pausedUntil) {
			continue
		}
		err := s.readTeam(ctx, t, done, &res)
		var rl *rateLimited
		s.mu.Lock()
		switch {
		case errors.As(err, &rl):
			t.pausedUntil = now.Add(rl.wait)
		case err != nil:
			t.lastErr = err.Error()
			errs = append(errs, fmt.Sprintf("%s: %v", t.label(), err))
		default:
			t.lastErr, t.lastOK = "", now
		}
		s.mu.Unlock()
	}
	if len(errs) > 0 {
		return res, errors.New(strings.Join(errs, "; "))
	}
	return res, nil
}

func (t *team) label() string {
	if t.account != "" {
		return t.account
	}
	if t.integration != "" {
		return t.integration
	}
	return "slack"
}

// team returns the state for a workspace login, starting it if new.
func (s *Source) team(w Workspace) *team {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.teams[w.UserToken]
	if !ok {
		t = &team{token: w.UserToken, convs: map[string]*conv{}, names: map[string]string{}}
		s.teams[w.UserToken] = t
	}
	t.integration, t.bot = w.Integration, strings.TrimSpace(w.BotToken)
	return t
}

// identify learns who the operator is in this workspace, and which bot is
// KARMAX's own.
func (s *Source) identify(ctx context.Context, t *team) error {
	s.mu.Lock()
	id, token, bot, botUser := t.id, t.token, t.bot, t.botUser
	s.mu.Unlock()

	if id == "" {
		var me struct {
			UserID string `json:"user_id"`
			Team   string `json:"team"`
			TeamID string `json:"team_id"`
			URL    string `json:"url"`
		}
		if err := s.call(ctx, token, "auth.test", nil, &me); err != nil {
			return err
		}
		base := strings.TrimSpace(me.URL)
		if base != "" && !strings.HasSuffix(base, "/") {
			base += "/"
		}
		account := strings.ToLower(me.TeamID)
		if u, err := url.Parse(base); err == nil && u.Host != "" {
			account, _, _ = strings.Cut(u.Host, ".")
		}
		s.mu.Lock()
		t.id, t.name, t.base, t.account, t.self = me.TeamID, me.Team, base, account, me.UserID
		s.mu.Unlock()
		s.userName(ctx, t, me.UserID) // so a mention of the operator reads as their name
	}

	// Without the bot's identity, KARMAX's own bot replying in a DM with the
	// operator would be heard as someone else — so a pause waits for it, and
	// only a token Slack refuses outright is read past, with a note.
	if bot != "" && botUser == "" {
		var b struct {
			UserID string `json:"user_id"`
			BotID  string `json:"bot_id"`
		}
		err := s.call(ctx, bot, "auth.test", nil, &b)
		var api *apiError
		switch {
		case err == nil:
			s.mu.Lock()
			t.botUser, t.botID, t.botNote = b.UserID, b.BotID, ""
			s.mu.Unlock()
		case errors.As(err, &api):
			s.mu.Lock()
			t.botNote = "the bot token was refused (" + api.Code + "), so KARMAX's own bot cannot be told apart; " +
				"DMs from bots are never urgent"
			s.mu.Unlock()
		default:
			return err
		}
	}
	return nil
}

func (s *Source) readTeam(ctx context.Context, t *team, done map[string]bool, res *Result) error {
	if err := s.identify(ctx, t); err != nil {
		return err
	}
	if done[t.id] {
		return nil // the same workspace through a second login
	}
	done[t.id] = true
	res.Workspaces++
	now := s.Now()
	if t.watchedSince.IsZero() {
		since, _, err := s.firstLook.Since(t.id, now)
		if err != nil {
			return err
		}
		t.watchedSince = since
	}
	if t.listedAt.IsZero() || now.Sub(t.listedAt) >= s.cfg.RefreshEvery {
		if err := s.list(ctx, t); err != nil {
			return err
		}
	}

	for _, c := range s.due(t, now) {
		if err := s.readConv(ctx, t, c, res); err != nil {
			return err
		}
		res.Read++
	}
	return s.searchMentions(ctx, t, res)
}

// list fetches the conversations the operator is in.
func (s *Source) list(ctx context.Context, t *team) error {
	deny := map[string]bool{}
	for _, d := range s.cfg.Deny {
		deny[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "#"))] = true
	}
	next := map[string]*conv{}
	cursor := ""
	for page := 0; page < 50; page++ {
		form := url.Values{"types": {"public_channel,private_channel,mpim,im"}, "exclude_archived": {"true"},
			"limit": {"200"}}
		if cursor != "" {
			form.Set("cursor", cursor)
		}
		var out struct {
			Channels []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				User      string `json:"user"`
				IsIM      bool   `json:"is_im"`
				IsMPIM    bool   `json:"is_mpim"`
				IsArchive bool   `json:"is_archived"`
			} `json:"channels"`
			Meta struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := s.call(ctx, t.token, "users.conversations", form, &out); err != nil {
			return err
		}
		for _, ch := range out.Channels {
			if ch.IsArchive || deny[strings.ToLower(ch.ID)] || (ch.Name != "" && deny[strings.ToLower(ch.Name)]) {
				continue
			}
			c := t.convs[ch.ID]
			if c == nil {
				c = &conv{id: ch.ID}
			}
			c.name, c.other = ch.Name, ch.User
			switch {
			case ch.IsIM:
				c.kind = "dm"
			case ch.IsMPIM:
				c.kind = "group_dm"
			default:
				c.kind = "channel"
			}
			next[ch.ID] = c
		}
		if cursor = out.Meta.Next; cursor == "" {
			break
		}
	}
	s.mu.Lock()
	t.convs, t.listedAt = next, s.Now()
	s.mu.Unlock()
	return nil
}

// due picks what this tick reads: direct messages first, then whatever has
// waited longest, up to the budget.
func (s *Source) due(t *team, now time.Time) []*conv {
	var out []*conv
	for _, c := range t.convs {
		if !c.nextDue.After(now) {
			out = append(out, c)
		}
	}
	rank := func(c *conv) int {
		if c.kind == "channel" {
			return 1
		}
		return 0
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if rank(a) != rank(b) {
			return rank(a) < rank(b)
		}
		if !a.nextDue.Equal(b.nextDue) {
			return a.nextDue.Before(b.nextDue)
		}
		return a.id < b.id
	})
	if len(out) > s.cfg.CallsPerTick {
		out = out[:s.cfg.CallsPerTick]
	}
	return out
}

// message is the part of a Slack message the source reads.
type message struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Username string `json:"username"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	Files    []struct {
		Name string `json:"name"`
	} `json:"files"`
	BotProfile struct {
		Name string `json:"name"`
	} `json:"bot_profile"`
}

func (s *Source) readConv(ctx context.Context, t *team, c *conv, res *Result) error {
	key := t.id + ":" + c.id
	cursor, have, err := s.kv.KVGet(cursorGroup, key)
	if err != nil {
		return err
	}
	now := s.Now()
	oldest := cursor
	// Never read before, so read back as far as the lookback: what is found
	// there from before KARMAX first looked at the workspace is backlog. A
	// conversation resumed from its cursor is being watched, and all news.
	fresh := !have || cursor == ""
	if fresh {
		oldest = timeTS(now.Add(-s.cfg.Lookback))
	} else {
		c.lastActive = tsTime(cursor)
	}

	var msgs []message
	page := ""
	for i := 0; i < 5; i++ {
		form := url.Values{"channel": {c.id}, "oldest": {oldest}, "limit": {"200"}}
		if page != "" {
			form.Set("cursor", page)
		}
		var out struct {
			Messages []message `json:"messages"`
			HasMore  bool      `json:"has_more"`
			Meta     struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := s.call(ctx, t.token, "conversations.history", form, &out); err != nil {
			var api *apiError
			if errors.As(err, &api) && (api.Code == "channel_not_found" || api.Code == "not_in_channel") {
				c.nextDue = now.Add(s.cfg.DormantEvery) // left or lost since the list was read
				return nil
			}
			return err
		}
		msgs = append(msgs, out.Messages...)
		if page = out.Meta.Next; !out.HasMore || page == "" {
			break
		}
	}

	// Oldest first, so events reach the agent in the order they were said.
	sort.Slice(msgs, func(i, j int) bool { return tsAfter(msgs[j].TS, msgs[i].TS) })
	newest := cursor
	var chatter []sources.Event
	for _, m := range msgs {
		if newest == "" || tsAfter(m.TS, newest) {
			newest = m.TS
		}
		e, ok := s.event(ctx, t, c, m)
		if !ok {
			continue
		}
		if fresh {
			e = e.WatchedSince(t.watchedSince)
		}
		if first, err := s.seen.First(e.Key); err != nil {
			return err
		} else if !first {
			continue
		}
		// A channel's ordinary talk goes out as one event per read; a DM or
		// a mention is someone asking and goes out on its own.
		if e.Type == "message.channel" {
			chatter = append(chatter, e)
			continue
		}
		if err := s.publish(e); err != nil {
			return err
		}
		res.Published++
	}
	if len(chatter) > 0 {
		if err := s.publish(batch(c, chatter)); err != nil {
			return err
		}
		res.Published++
	}
	if newest != "" && newest != cursor {
		if err := s.kv.KVSet(cursorGroup, key, newest, 0); err != nil {
			return err
		}
		c.lastActive = tsTime(newest)
	}
	if !c.lastActive.IsZero() && now.Sub(c.lastActive) < s.cfg.ActiveWindow {
		c.nextDue = now
	} else {
		c.nextDue = now.Add(s.cfg.DormantEvery)
	}
	return nil
}

// batch folds a channel's new messages into one event: the last message's,
// with every line in the summary and the messages in its data.
func batch(c *conv, evs []sources.Event) sources.Event {
	if len(evs) == 1 {
		return evs[0]
	}
	first, last := evs[0], evs[len(evs)-1]
	lines := make([]string, 0, len(evs))
	msgs := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		lines = append(lines, fmt.Sprintf("%v: %v", e.Data["user_name"], e.Data["said"]))
		msgs = append(msgs, map[string]any{"user": e.Data["user"], "user_name": e.Data["user_name"],
			"ts": e.Data["ts"], "thread_ts": e.Data["thread_ts"], "text": e.Data["text"], "url": e.URL})
	}
	data := make(map[string]any, len(last.Data)+2)
	for k, v := range last.Data {
		data[k] = v
	}
	data["messages"], data["count"] = msgs, len(evs)
	data["thread_ts"] = "" // an answer to the conversation goes in the channel

	e := last
	e.Data, e.URL = data, first.URL
	e.Title = fmt.Sprintf("#%s — %d new messages; last from %v: %s", c.name, len(evs), last.Data["user_name"],
		clip(firstLine(fmt.Sprint(last.Data["said"])), 120))
	e.Summary = clip(fmt.Sprintf("New in #%s on the %v Slack:\n%s", c.name, firstNonEmpty(fmt.Sprint(last.Data["team"]),
		fmt.Sprint(last.Data["workspace"])), strings.Join(lines, "\n")), 6000)
	return e
}

// offer publishes a message if it is an event and new. fresh is a read with
// no cursor to resume from, whose finds from before the first look are
// backlog.
func (s *Source) offer(ctx context.Context, t *team, c *conv, m message, fresh bool, res *Result) error {
	e, ok := s.event(ctx, t, c, m)
	if !ok {
		return nil
	}
	if fresh {
		e = e.WatchedSince(t.watchedSince)
	}
	first, err := s.seen.First(e.Key)
	if err != nil || !first {
		return err
	}
	if err := s.publish(e); err != nil {
		return err
	}
	res.Published++
	return nil
}

// event turns a message into an Event, or reports that it is not one.
func (s *Source) event(ctx context.Context, t *team, c *conv, m message) (sources.Event, bool) {
	switch m.Subtype {
	case "", "bot_message", "thread_broadcast", "file_share", "me_message":
	default:
		return sources.Event{}, false // joins, edits, deletions, topic changes
	}
	if m.TS == "" || (m.User != "" && m.User == t.self) {
		return sources.Event{}, false
	}
	if (t.botUser != "" && m.User == t.botUser) || (t.botID != "" && m.BotID == t.botID) {
		return sources.Event{}, false
	}
	text := strings.TrimSpace(m.Text)
	if text == "" && len(m.Files) > 0 {
		text = "[shared a file: " + m.Files[0].Name + "]"
	}
	if text == "" {
		return sources.Event{}, false
	}

	who := ""
	if m.User != "" {
		who = s.userName(ctx, t, m.User)
	}
	who = firstNonEmpty(who, m.Username, m.BotProfile.Name, "someone")
	readable := s.readable(ctx, t, text)
	snippet := clip(firstLine(readable), 140)
	mentioned := t.self != "" && (strings.Contains(text, "<@"+t.self+">") || strings.Contains(text, "<@"+t.self+"|"))

	var typ, title, where string
	urgent := true
	reply := m.ThreadTS
	switch {
	case c.kind == "dm":
		typ, where = "message.dm", "a direct message"
		title = fmt.Sprintf("Slack DM from %s: %s", who, snippet)
		// Slackbot's reminders and notices are not a person asking.
		urgent = m.User != "USLACKBOT" && m.BotID == "" && m.Subtype != "bot_message"
	case c.kind == "group_dm":
		typ, where = "message.group_dm", "a group DM"
		title = fmt.Sprintf("Slack group DM from %s: %s", who, snippet)
	case mentioned:
		typ, where = "message.mention", "#"+c.name
		title = fmt.Sprintf("%s mentioned you in #%s: %s", who, c.name, snippet)
		if reply == "" {
			reply = m.TS
		}
	default:
		typ, where, urgent = "message.channel", "#"+c.name, false
		title = fmt.Sprintf("#%s — %s: %s", c.name, who, snippet)
		if reply == "" {
			reply = m.TS
		}
	}
	summary := fmt.Sprintf("%s wrote in %s on the %s Slack:\n%s", who, where, firstNonEmpty(t.name, t.account), clip(readable, 4000))

	return sources.Event{
		Source:  Name,
		Account: t.account,
		Type:    typ,
		Key:     t.id + ":" + c.id + ":" + m.TS,
		Title:   title,
		Summary: summary,
		URL:     permalink(t.base, c.id, m.TS, m.ThreadTS),
		At:      tsTime(m.TS),
		Urgent:  urgent,
		Data: map[string]any{
			"workspace":    t.account,
			"team":         t.name,
			"team_id":      t.id,
			"integration":  t.integration,
			"channel":      c.id,
			"channel_name": c.name,
			"conversation": c.kind,
			"user":         m.User,
			"user_name":    who,
			"ts":           m.TS,
			"thread_ts":    reply,
			"text":         text,
			"said":         clip(readable, 1000),
			"mentioned":    mentioned,
			"reply_with":   "slack.send_as_operator with this workspace, channel and thread_ts",
		},
	}, true
}

// searchMentions finds the operator's mentions that history cannot see:
// replies inside threads, and channels read less often.
func (s *Source) searchMentions(ctx context.Context, t *team, res *Result) error {
	q := strings.TrimSpace(s.cfg.MentionQuery)
	if t.searchOff != "" || strings.EqualFold(q, "off") || t.self == "" {
		return nil
	}
	var out struct {
		Messages struct {
			Matches []struct {
				message
				Channel struct {
					ID     string `json:"id"`
					Name   string `json:"name"`
					IsIM   bool   `json:"is_im"`
					IsMPIM bool   `json:"is_mpim"`
				} `json:"channel"`
				Permalink string `json:"permalink"`
			} `json:"matches"`
		} `json:"messages"`
	}
	form := url.Values{"query": {strings.ReplaceAll(q, "{user}", t.self)}, "sort": {"timestamp"},
		"sort_dir": {"desc"}, "count": {"50"}}
	if err := s.call(ctx, t.token, "search.messages", form, &out); err != nil {
		var api *apiError
		if errors.As(err, &api) && (api.Code == "missing_scope" || api.Code == "not_allowed_token_type") {
			s.mu.Lock()
			t.searchOff = "mentions inside threads are not seen: the user token lacks search:read"
			s.mu.Unlock()
			return nil
		}
		return err
	}

	key := t.id + ":search"
	cursor, have, err := s.kv.KVGet(cursorGroup, key)
	if err != nil {
		return err
	}
	fresh := !have || cursor == ""
	if fresh {
		cursor = timeTS(s.Now().Add(-s.cfg.Lookback))
	}
	matches := out.Messages.Matches
	sort.Slice(matches, func(i, j int) bool { return tsAfter(matches[j].TS, matches[i].TS) })
	newest := cursor
	for _, m := range matches {
		if !tsAfter(m.TS, cursor) {
			continue
		}
		if tsAfter(m.TS, newest) {
			newest = m.TS
		}
		c := t.convs[m.Channel.ID]
		if c == nil {
			c = &conv{id: m.Channel.ID, name: m.Channel.Name, kind: "channel"}
			if m.Channel.IsIM {
				c.kind = "dm"
			} else if m.Channel.IsMPIM {
				c.kind = "group_dm"
			}
		}
		msg := m.message
		if msg.ThreadTS == "" {
			if u, err := url.Parse(m.Permalink); err == nil {
				msg.ThreadTS = u.Query().Get("thread_ts")
			}
		}
		if err := s.offer(ctx, t, c, msg, fresh, res); err != nil {
			return err
		}
	}
	if newest != cursor {
		return s.kv.KVSet(cursorGroup, key, newest, 0)
	}
	return nil
}

// userName resolves a Slack user id to a name, cached per workspace.
func (s *Source) userName(ctx context.Context, t *team, id string) string {
	if id == "" {
		return ""
	}
	s.mu.Lock()
	n, ok := t.names[id]
	s.mu.Unlock()
	if ok {
		return n
	}
	var out struct {
		User struct {
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	n = id // the id is a worse name than a name and a better one than nothing
	if err := s.call(ctx, t.token, "users.info", url.Values{"user": {id}}, &out); err == nil {
		n = firstNonEmpty(out.User.Profile.RealName, out.User.Profile.DisplayName, out.User.Name, id)
	}
	s.mu.Lock()
	t.names[id] = n
	s.mu.Unlock()
	return n
}

var (
	refRe  = regexp.MustCompile(`<([@#!])([^>|]+)(?:\|([^>]*))?>`)
	linkRe = regexp.MustCompile(`<((?:https?|mailto):[^>|]+)(?:\|([^>]*))?>`)
)

// readable turns Slack's markup into what a person sees: <@U1> becomes
// @Name, <#C1|eng> becomes #eng, a link becomes its label.
func (s *Source) readable(ctx context.Context, t *team, text string) string {
	text = linkRe.ReplaceAllStringFunc(text, func(m string) string {
		p := linkRe.FindStringSubmatch(m)
		return firstNonEmpty(p[2], p[1])
	})
	text = refRe.ReplaceAllStringFunc(text, func(m string) string {
		p := refRe.FindStringSubmatch(m)
		switch p[1] {
		case "@":
			return "@" + firstNonEmpty(p[3], s.userName(ctx, t, p[2]))
		case "#":
			return "#" + firstNonEmpty(p[3], p[2])
		default:
			if p[3] != "" {
				return p[3]
			}
			return "@" + p[2]
		}
	})
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(text)
}

// Status reports each workspace, for health.
func (s *Source) Status() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Status
	for _, t := range s.teams {
		var notes []string
		for _, n := range []string{t.searchOff, t.botNote} {
			if n != "" {
				notes = append(notes, n)
			}
		}
		out = append(out, Status{
			Integration: t.integration, Account: t.account, Team: t.name, Conversations: len(t.convs),
			LastOK: t.lastOK, LastError: t.lastErr, PausedUntil: t.pausedUntil, Note: strings.Join(notes, "; "),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
