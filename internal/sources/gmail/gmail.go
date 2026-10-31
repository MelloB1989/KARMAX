// Package gmail turns new mail in one Google account's inbox into
// sources.Event: every message a person or a system sent to the operator
// that Gmail did not file as bulk, urgent when a person writes to them
// directly about something that matters.
//
// It is pure logic. How history and messages are read is the API seam, where
// the history id lives is the Store seam, and what was already told is
// sources.Seen.
//
// Mail is sensitive, so an event carries what Gmail shows in a list — the
// sender, the subject, the snippet, the attachments' names — and never a
// body. The agent reads a body through a tool when it needs one.
package gmail

import (
	"context"
	"errors"
	"fmt"
	"html"
	"mime"
	netmail "net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

// Name is the source's name on its events.
const Name = "gmail"

// Header is one message header.
type Header struct{ Name, Value string }

// Ref is a message as a history record or a listing names it.
type Ref struct {
	ID       string
	ThreadID string
	// Labels are the message's label ids; nil when the listing does not say.
	Labels []string
}

// Message is one message's labels, headers and snippet — never its body.
type Message struct {
	ID       string
	ThreadID string
	Labels   []string
	Snippet  string
	At       time.Time
	Headers  []Header
	// Attachments are the attached files' names, when the path can tell.
	Attachments []string
}

// Header is the first value of a header, "" when there is none.
func (m Message) Header(name string) string {
	for _, h := range m.Headers {
		if strings.EqualFold(h.Name, name) {
			return strings.TrimSpace(h.Value)
		}
	}
	return ""
}

func (m Message) has(label string) bool { return hasLabel(m.Labels, label) }

func hasLabel(labels []string, label string) bool {
	for _, l := range labels {
		if l == label {
			return true
		}
	}
	return false
}

// Page is what a history listing found.
type Page struct {
	// Added are the messages added since the start, oldest first.
	Added []Ref
	// Next is the history id to list from next time.
	Next string
}

var (
	// ErrHistoryExpired is returned when Google no longer keeps the history
	// a listing starts from; the newest inbox mail is read instead.
	ErrHistoryExpired = errors.New("gmail: the history id has expired")
	// ErrGone is returned for a message that no longer exists.
	ErrGone = errors.New("gmail: the message no longer exists")
)

// API reads one account's mail.
type API interface {
	// Start is the account's current history id: where watching begins.
	Start(ctx context.Context, account string) (string, error)
	// History lists the messages added since start.
	History(ctx context.Context, account, start string) (Page, error)
	// Recent lists the newest messages in the inbox, newest first.
	Recent(ctx context.Context, account string, max int) ([]Ref, error)
	// Message reads one message's labels, headers and snippet.
	Message(ctx context.Context, account, id string) (Message, error)
	// Thread lists a thread's messages with their labels, nothing more.
	Thread(ctx context.Context, account, threadID string) ([]Ref, error)
}

// State is what is remembered about one account between ticks.
type State struct {
	FirstSeen time.Time `json:"first_seen"`
	HistoryID string    `json:"history_id,omitempty"`
	LastTick  time.Time `json:"last_tick"`
	// Failing counts the ticks a message could not be read in.
	Failing map[string]int `json:"failing,omitempty"`
}

// Store keeps State per account.
type Store interface {
	Load(account string) (State, bool, error)
	Save(account string, st State) error
}

// DefaultSkipCategories are the Gmail tabs whose mail is never told: what
// Gmail itself files away from the operator's primary inbox.
var DefaultSkipCategories = []string{"promotions", "social", "updates", "forums"}

// DefaultAutomated are words in a sender's address, before the @, that mark
// mail nobody reads replies to.
var DefaultAutomated = []string{"noreply", "donotreply"}

// Urgency rules: with a person writing to the operator directly, the message
// is urgent when …
const (
	UrgentImportant = "important"  // … Gmail marks it important
	UrgentOwnThread = "own-thread" // … it answers a thread the operator wrote in
	UrgentDirect    = "direct"     // … always
)

// DefaultUrgentWhen are the urgency rules used unless configured.
var DefaultUrgentWhen = []string{UrgentImportant, UrgentOwnThread}

// Config tunes the watcher. Zero values take the defaults; an empty, non-nil
// list means none.
type Config struct {
	Location *time.Location
	// FirstLook is how far back an account's first look reads unread mail (24h).
	FirstLook time.Duration
	// MaxPerTick bounds the messages read in one tick (40); a bigger burst is
	// read over the next ticks.
	MaxPerTick int
	// SkipCategories are the Gmail categories never told, by name
	// ("promotions") or label (CATEGORY_PROMOTIONS).
	SkipCategories []string
	// Automated are the sender address words that mark automated mail.
	Automated []string
	// UrgentWhen are the urgency rules (UrgentImportant, UrgentOwnThread,
	// UrgentDirect).
	UrgentWhen []string
	// Own lists the operator's other addresses, so mail from any of them is
	// the operator's own and not a person writing to them.
	Own func() []string
}

func (c Config) withDefaults() Config {
	if c.Location == nil {
		c.Location = time.UTC
	}
	if c.FirstLook <= 0 {
		c.FirstLook = 24 * time.Hour
	}
	if c.MaxPerTick <= 0 {
		c.MaxPerTick = 40
	}
	if c.SkipCategories == nil {
		c.SkipCategories = DefaultSkipCategories
	}
	if c.Automated == nil {
		c.Automated = DefaultAutomated
	}
	if c.UrgentWhen == nil {
		c.UrgentWhen = DefaultUrgentWhen
	}
	return c
}

// CategoryLabel is the label Gmail files a category under: "promotions" is
// CATEGORY_PROMOTIONS.
func CategoryLabel(name string) string {
	n := strings.ToUpper(strings.TrimSpace(name))
	if n == "" || strings.HasPrefix(n, "CATEGORY_") {
		return n
	}
	return "CATEGORY_" + n
}

// Watcher watches inboxes.
type Watcher struct {
	cfg    Config
	api    API
	store  Store
	seen   sources.Seen
	now    func() time.Time
	skip   map[string]bool
	urgent map[string]bool
}

// New builds a watcher.
func New(cfg Config, api API, store Store, seen sources.Seen) *Watcher {
	cfg = cfg.withDefaults()
	w := &Watcher{cfg: cfg, api: api, store: store, seen: seen, now: time.Now,
		skip: map[string]bool{}, urgent: map[string]bool{}}
	for _, c := range cfg.SkipCategories {
		if l := CategoryLabel(c); l != "" {
			w.skip[l] = true
		}
	}
	for _, u := range cfg.UrgentWhen {
		w.urgent[strings.ReplaceAll(strings.ToLower(strings.TrimSpace(u)), "_", "-")] = true
	}
	return w
}

// Result says what one tick did.
type Result struct {
	First   bool // the account's first look
	Expired bool // the history id had expired, so the newest inbox mail was read
	More    bool // more was added than one tick reads; the rest comes next tick
	Listed  int  // messages the history or listing named
	Read    int  // messages whose headers were read
	// Skipped counts what was not told, by why: own, draft, spam, trash,
	// not-inbox, category, bulk, read, old, unreadable.
	Skipped   map[string]int
	Published int
	Urgent    int
	Backlog   int
	// Failed are messages that could not be read; tried again next tick.
	Failed []string
}

// maxAttempts is how many ticks a message that cannot be read is tried in
// before it is given up.
const maxAttempts = 5

// clockSlack absorbs the difference between this host's clock and Google's.
const clockSlack = 2 * time.Minute

// Tick reads one account's new mail and publishes it.
//
// The history id moves on only once everything it covers was read, so a
// failure means the same history is read again next tick; what was told is
// told once, through Seen.
func (w *Watcher) Tick(ctx context.Context, account string, publish func(sources.Event) error) (Result, error) {
	now := w.now()
	st, found, err := w.store.Load(account)
	if err != nil {
		return Result{}, fmt.Errorf("gmail %s: load state: %w", account, err)
	}
	res := Result{Skipped: map[string]int{}}
	if st.Failing == nil {
		st.Failing = map[string]int{}
	}

	// since is how recent a listed (not history) message must be to be told;
	// unread is whether it must also be unread.
	var (
		refs   []Ref
		next   string
		since  time.Time
		unread bool
	)
	switch {
	case !found || st.HistoryID == "":
		res.First = true
		if st.FirstSeen.IsZero() {
			st.FirstSeen = now
		}
		// The history id first, then the listing: whatever arrives between
		// the two is in both, and Seen tells it once.
		if next, err = w.api.Start(ctx, account); err != nil {
			return res, err
		}
		if refs, err = w.recent(ctx, account); err != nil {
			return res, err
		}
		since, unread = st.FirstSeen.Add(-w.cfg.FirstLook), true
	default:
		page, herr := w.api.History(ctx, account, st.HistoryID)
		switch {
		case errors.Is(herr, ErrHistoryExpired):
			res.Expired = true
			if next, err = w.api.Start(ctx, account); err != nil {
				return res, err
			}
			if refs, err = w.recent(ctx, account); err != nil {
				return res, err
			}
			since = st.LastTick.Add(-clockSlack)
		case herr != nil:
			return res, herr
		default:
			refs, next = page.Added, page.Next
		}
	}
	res.Listed = len(refs)

	own := w.own(account)
	hold := false
	for _, ref := range refs {
		if ref.Labels != nil {
			// The listing already says what the message is: what it rules
			// out costs no read.
			if reason := w.ruledOut(ref.Labels); reason != "" {
				res.Skipped[reason]++
				continue
			}
		}
		key := seenKey(account, ref.ID)
		if _, told, err := w.seen.Get(w.seen.Group, key); err != nil {
			return res, fmt.Errorf("gmail %s: seen: %w", account, err)
		} else if told {
			continue
		}
		if res.Read >= w.cfg.MaxPerTick {
			res.More, hold = true, true
			break
		}
		m, err := w.api.Message(ctx, account, ref.ID)
		switch {
		case errors.Is(err, ErrGone):
			continue
		case err != nil:
			st.Failing[ref.ID]++
			if st.Failing[ref.ID] < maxAttempts {
				res.Failed = append(res.Failed, ref.ID)
				hold = true
				continue
			}
			delete(st.Failing, ref.ID)
			res.Skipped["unreadable"]++
			if _, err := w.seen.First(key); err != nil {
				return res, fmt.Errorf("gmail %s: seen: %w", account, err)
			}
			continue
		}
		delete(st.Failing, ref.ID)
		res.Read++
		if m.ID == "" {
			m.ID = ref.ID
		}
		if m.ThreadID == "" {
			m.ThreadID = ref.ThreadID
		}

		reason := w.ruledOut(m.Labels)
		switch {
		case reason != "":
		case own[address(m.Header("From"))]:
			reason = "own"
		case w.bulk(m):
			reason = "bulk"
		case !since.IsZero() && !m.At.After(since):
			reason = "old"
		case unread && !m.has("UNREAD"):
			reason = "read"
		}
		if reason != "" {
			res.Skipped[reason]++
			if reason != "old" && reason != "read" {
				// Never to be told, so never read again.
				if _, err := w.seen.First(key); err != nil {
					return res, fmt.Errorf("gmail %s: seen: %w", account, err)
				}
			}
			continue
		}

		inThread := false
		if m.ThreadID != "" && m.ThreadID != m.ID {
			// A thread's id is its first message's, so only a later
			// message can answer something the operator wrote.
			if thread, err := w.api.Thread(ctx, account, m.ThreadID); err == nil {
				for _, r := range thread {
					if r.ID != m.ID && hasLabel(r.Labels, "SENT") {
						inThread = true
						break
					}
				}
			}
		}
		// Gmail dates mail by its own clock: what it received just after the
		// first look must not read as before it on this host's.
		e := w.event(account, own, m, inThread).WatchedSince(st.FirstSeen.Add(-clockSlack))
		if res.First {
			e.Backlog = true
		}
		if e.Backlog {
			e.Urgent = false // context, never a page
		}

		isNew, err := w.seen.First(key)
		if err != nil {
			return res, fmt.Errorf("gmail %s: seen: %w", account, err)
		}
		if !isNew {
			continue
		}
		if err := publish(e); err != nil {
			return res, fmt.Errorf("gmail %s: publish: %w", account, err)
		}
		res.Published++
		if e.Urgent {
			res.Urgent++
		}
		if e.Backlog {
			res.Backlog++
		}
	}

	// A first look or a fallback that could not read everything still
	// starts watching from now: what it missed was listed, not news.
	if !hold || res.First || res.Expired {
		st.HistoryID = next
	}
	st.LastTick = now
	if err := w.store.Save(account, st); err != nil {
		return res, fmt.Errorf("gmail %s: save state: %w", account, err)
	}
	return res, nil
}

// recent is the newest inbox mail, oldest first.
func (w *Watcher) recent(ctx context.Context, account string) ([]Ref, error) {
	refs, err := w.api.Recent(ctx, account, w.cfg.MaxPerTick)
	if err != nil {
		return nil, err
	}
	out := make([]Ref, 0, len(refs))
	for i := len(refs) - 1; i >= 0; i-- {
		out = append(out, refs[i])
	}
	return out, nil
}

func seenKey(account, id string) string {
	return "gmail|" + strings.ToLower(account) + "|" + id
}

// ruledOut says why labels alone rule a message out, or "".
func (w *Watcher) ruledOut(labels []string) string {
	switch {
	case hasLabel(labels, "SENT"):
		return "own"
	case hasLabel(labels, "DRAFT"):
		return "draft"
	case hasLabel(labels, "SPAM"):
		return "spam"
	case hasLabel(labels, "TRASH"):
		return "trash"
	case !hasLabel(labels, "INBOX"):
		return "not-inbox"
	}
	for _, l := range labels {
		if w.skip[l] {
			return "category"
		}
	}
	return ""
}

// own is the operator's addresses: the account and any other they have.
func (w *Watcher) own(account string) map[string]bool {
	out := map[string]bool{strings.ToLower(strings.TrimSpace(account)): true}
	if w.cfg.Own != nil {
		for _, a := range w.cfg.Own() {
			if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
				out[a] = true
			}
		}
	}
	return out
}

// bulk reports mail sent to many by Gmail's and the sender's own signals: an
// unsubscribe link, a bulk or list precedence, a sender that takes no reply.
func (w *Watcher) bulk(m Message) bool {
	if m.Header("List-Unsubscribe") != "" {
		return true
	}
	switch strings.ToLower(m.Header("Precedence")) {
	case "bulk", "list", "junk":
		return true
	}
	local, _, _ := strings.Cut(address(m.Header("From")), "@")
	local = strings.NewReplacer("-", "", "_", "", ".", "").Replace(local)
	for _, word := range w.cfg.Automated {
		word = strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(strings.TrimSpace(word)))
		if word != "" && strings.Contains(local, word) {
			return true
		}
	}
	return false
}

// automated reports mail a program sent rather than a person: an
// auto-reply, a bounce, a vacation notice.
func automated(m Message) bool {
	if a := strings.ToLower(m.Header("Auto-Submitted")); a != "" && a != "no" {
		return true
	}
	if strings.EqualFold(m.Header("Precedence"), "auto_reply") {
		return true
	}
	if m.Header("X-Autoreply") != "" || m.Header("X-Autorespond") != "" {
		return true
	}
	return m.Header("Return-Path") == "<>"
}

var words = new(mime.WordDecoder)

func decode(s string) string {
	if d, err := words.DecodeHeader(s); err == nil {
		s = d
	}
	return strings.Join(strings.Fields(s), " ")
}

// address is the bare, lower-cased address in a From-style header.
func address(header string) string {
	if a, err := netmail.ParseAddress(header); err == nil {
		return strings.ToLower(a.Address)
	}
	if i, j := strings.LastIndex(header, "<"), strings.LastIndex(header, ">"); i >= 0 && j > i {
		header = header[i+1 : j]
	}
	return strings.ToLower(strings.Trim(strings.TrimSpace(header), `"'`))
}

// person names who a From-style header is: the display name, else the address.
func person(header string) (name, addr string) {
	if a, err := netmail.ParseAddress(header); err == nil {
		return strings.TrimSpace(a.Name), strings.ToLower(a.Address)
	}
	addr = address(header)
	if i := strings.LastIndex(header, "<"); i > 0 {
		name = strings.Trim(strings.TrimSpace(decode(header[:i])), `"'`)
	}
	return name, addr
}

// recipients splits a To- or Cc-style header into "Name <address>" entries.
func recipients(header string) []string {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	var out []string
	if list, err := netmail.ParseAddressList(header); err == nil {
		for _, a := range list {
			out = append(out, render(a.Name, a.Address))
		}
		return out
	}
	for _, part := range strings.Split(header, ",") {
		if part = strings.TrimSpace(part); part != "" {
			name, addr := person(part)
			out = append(out, render(name, addr))
		}
	}
	return out
}

func render(name, addr string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return strings.ToLower(addr)
	}
	return name + " <" + strings.ToLower(addr) + ">"
}

// addressed reports whether any of the operator's addresses is among a
// message's recipients.
func addressed(own map[string]bool, lists ...[]string) bool {
	for _, list := range lists {
		for _, r := range list {
			if own[address(r)] {
				return true
			}
		}
	}
	return false
}

// URL opens a thread in Gmail as the account.
func URL(account, threadID string) string {
	return "https://mail.google.com/mail/?authuser=" + url.QueryEscape(strings.ToLower(account)) + "#all/" + threadID
}

func (w *Watcher) event(account string, own map[string]bool, m Message, inThread bool) sources.Event {
	from := decode(m.Header("From"))
	name, addr := person(from)
	who := name
	if who == "" {
		who = addr
	}
	if who == "" {
		who = "someone"
	}
	subject := decode(m.Header("Subject"))
	if subject == "" {
		subject = "(no subject)"
	}
	to, cc := recipients(decode(m.Header("To"))), recipients(decode(m.Header("Cc")))
	direct := addressed(own, to, cc)
	important := m.has("IMPORTANT")
	bot := automated(m)

	e := sources.Event{Source: Name, Account: strings.ToLower(account), Type: "message", Key: m.ID,
		Title: fmt.Sprintf("Email from %s: %s", who, subject), Summary: strings.TrimSpace(html.UnescapeString(m.Snippet)),
		URL: URL(account, m.ThreadID), At: m.At}
	if inThread {
		e.Type = "reply"
		e.Title = fmt.Sprintf("%s replied: %s", who, subject)
	}
	if !bot && direct {
		e.Urgent = w.urgent[UrgentDirect] || (w.urgent[UrgentImportant] && important) || (w.urgent[UrgentOwnThread] && inThread)
	}

	e.Data = map[string]any{
		"thread":             m.ThreadID,
		"message":            m.ID,
		"from":               render(name, addr),
		"from_address":       addr,
		"email_subject":      subject,
		"to":                 to,
		"labels":             m.Labels,
		"addressed_directly": direct,
		"important":          important,
		"operator_in_thread": inThread,
		"unread":             m.has("UNREAD"),
		// What long-term memory files a fact from this mail under: the
		// person who wrote it, beside everything else known about them.
		"person":   who,
		"category": "people",
	}
	if len(cc) > 0 {
		e.Data["cc"] = cc
	}
	if len(m.Attachments) > 0 {
		e.Data["attachments"] = m.Attachments
	}
	if bot {
		e.Data["automated"] = true
	}
	return e
}
