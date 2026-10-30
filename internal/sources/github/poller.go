package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	ghconn "github.com/MelloB1989/karmax/internal/connectors/github"
	"github.com/MelloB1989/karmax/internal/sources"
)

const (
	stateGroup = "github-source"
	seenGroup  = "github-source-seen"
	// seenTTL outlives the overlap many times over; a key older than the
	// floor is never published again whether or not it is remembered.
	seenTTL = 14 * 24 * time.Hour
	// overlap re-reads the hour before the newest thread seen, for a thread
	// GitHub lists late. A re-read thread is deduplicated, not republished.
	overlap = time.Hour
	// maxPages bounds one account's tick. A day away is rarely more than one.
	maxPages = 5
	// maxDetails bounds the extra reads (subject, latest comment) per account
	// per tick; beyond it an event is published from the notification alone.
	maxDetails = 30
)

// KV is where the poller keeps its place: the store's KV in production.
type KV struct {
	Get func(group, key string) (string, bool, error)
	Set func(group, key, value string, ttl time.Duration) error
}

// Poller turns every gh account's new notifications into events.
type Poller struct {
	Logins  ghconn.Logins
	KV      KV
	Publish func(sources.Event) error
	HTTP    *http.Client
	Now     func() time.Time
	// FirstWindow is how far back an account seen for the first time is
	// published; anything older is marked seen. Default 24h.
	FirstWindow time.Duration
	// PerPage is the notifications page size. Default 50.
	PerPage int
}

// AccountResult is how one account's tick went.
type AccountResult struct {
	Account     string
	Login       ghconn.Account
	Published   int
	NotModified bool // GitHub said nothing changed
	Waiting     bool // GitHub's poll interval had not passed
	Err         error
}

// Result is one tick, every account.
type Result struct{ Accounts []AccountResult }

// Published counts every account's events.
func (r Result) Published() int {
	n := 0
	for _, a := range r.Accounts {
		n += a.Published
	}
	return n
}

// Failed lists the accounts that could not be read.
func (r Result) Failed() []AccountResult {
	var out []AccountResult
	for _, a := range r.Accounts {
		if a.Err != nil {
			out = append(out, a)
		}
	}
	return out
}

// state is one account's place, kept across restarts.
type state struct {
	// LastModified is the first page's Last-Modified, sent back as
	// If-Modified-Since.
	LastModified string `json:"last_modified,omitempty"`
	// NextPoll is when GitHub's X-Poll-Interval allows the next read.
	NextPoll time.Time `json:"next_poll,omitempty"`
	// Floor is the oldest update that may still be published. It only moves
	// forward, so a thread from before it — however it resurfaces — is never
	// news.
	Floor time.Time `json:"floor"`
}

// Tick reads every account once. An account that fails is reported in the
// result and never stops the rest; the tick fails only when accounts could
// not be listed, or none of them could be read.
func (p *Poller) Tick(ctx context.Context) (Result, error) {
	accts, err := p.Logins.Accounts(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("listing GitHub accounts: %w", err)
	}
	var res Result
	for _, a := range accts {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		r := AccountResult{Account: a.Name(), Login: a}
		if !a.OK() {
			r.Err = fmt.Errorf("gh reports the login as %s: %s", a.State, a.Error)
		} else {
			r = p.account(ctx, a)
		}
		res.Accounts = append(res.Accounts, r)
	}
	if failed := res.Failed(); len(failed) > 0 && len(failed) == len(res.Accounts) {
		var msgs []string
		for _, f := range failed {
			msgs = append(msgs, f.Account+": "+f.Err.Error())
		}
		return res, fmt.Errorf("no GitHub account could be read: %s", strings.Join(msgs, "; "))
	}
	return res, nil
}

func (p *Poller) account(ctx context.Context, a ghconn.Account) AccountResult {
	r := AccountResult{Account: a.Name(), Login: a}
	now := p.now()
	st, first, err := p.load(a.Name())
	if err != nil {
		r.Err = err
		return r
	}
	if now.Before(st.NextPoll) {
		r.Waiting = true
		return r
	}
	if first {
		st.Floor = now.Add(-p.firstWindow())
	}

	token, err := p.Logins.Token(ctx, a)
	if err != nil {
		r.Err = err
		return r
	}
	c := client{base: a.APIBase(), token: token, http: p.HTTP}

	ims := st.LastModified
	if first {
		ims = ""
	}
	url := fmt.Sprintf("%s/notifications?all=true&per_page=%d", a.APIBase(), p.perPage())
	pg, err := c.notifications(ctx, url, ims)
	if err != nil {
		var rl *RateLimitError
		if errors.As(err, &rl) {
			st.NextPoll = rl.Reset
			_ = p.save(a.Name(), st)
		}
		r.Err = err
		return r
	}
	st.NextPoll = now.Add(pg.pollInterval)
	if pg.notModified {
		r.NotModified = true
		r.Err = p.save(a.Name(), st)
		return r
	}
	lastModified := pg.lastModified

	// Newest first, so a page whose oldest thread is already behind the floor
	// is the last one worth reading.
	items := pg.items
	for pages := 1; pg.next != "" && pages < maxPages; pages++ {
		if len(pg.items) == 0 || pg.items[len(pg.items)-1].UpdatedAt.Before(st.Floor) {
			break
		}
		if pg, err = c.notifications(ctx, pg.next, ""); err != nil {
			r.Err = err
			return r
		}
		items = append(items, pg.items...)
	}

	seen := sources.Seen{Group: seenGroup, TTL: seenTTL, Get: p.KV.Get, Set: p.KV.Set}
	var fresh []Notification
	newest := st.Floor
	for _, n := range items {
		if n.UpdatedAt.After(newest) {
			newest = n.UpdatedAt
		}
		key := a.Name() + "/" + n.Key()
		if n.UpdatedAt.Before(st.Floor) {
			// Older than this account's floor: not news, now or later.
			if _, err := seen.First(key); err != nil {
				r.Err = err
				return r
			}
			continue
		}
		if _, ok, err := seen.Get(seen.Group, key); err != nil {
			r.Err = err
			return r
		} else if !ok {
			fresh = append(fresh, n)
		}
	}
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].UpdatedAt.Before(fresh[j].UpdatedAt) })

	for i, n := range fresh {
		var d Detail
		if i < maxDetails {
			d = p.detail(ctx, c, n)
		}
		// Published, then marked: a crash between the two repeats an event
		// rather than losing one.
		if err := p.Publish(Event(a.Name(), n, d)); err != nil {
			r.Err = fmt.Errorf("publishing %s: %w", n.Key(), err)
			return r
		}
		if err := seen.Set(seen.Group, a.Name()+"/"+n.Key(), "1", seen.TTL); err != nil {
			r.Err = err
			return r
		}
		r.Published++
	}

	if f := newest.Add(-overlap); f.After(st.Floor) {
		st.Floor = f
	}
	st.LastModified = lastModified
	r.Err = p.save(a.Name(), st)
	return r
}

// detail reads the subject and its latest comment. Either may fail; the event
// is published with what was read.
func (p *Poller) detail(ctx context.Context, c client, n Notification) Detail {
	var d Detail
	if u := n.Subject.URL; u != "" {
		var s struct {
			HTMLURL string `json:"html_url"`
			State   string `json:"state"`
			Merged  bool   `json:"merged"`
			Draft   bool   `json:"draft"`
			Number  int    `json:"number"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
		}
		if err := c.get(ctx, u, &s); err == nil {
			d.HTMLURL, d.State, d.Number, d.Author = s.HTMLURL, s.State, s.Number, s.User.Login
			if d.Author == "" {
				d.Author = s.Author.Login
			}
			switch {
			case s.Merged:
				d.State = "merged"
			case s.Draft && s.State == "open":
				d.State = "draft"
			}
		}
	}
	if u := n.Subject.LatestCommentURL; u != "" && u != n.Subject.URL {
		var cm struct {
			HTMLURL string `json:"html_url"`
			Body    string `json:"body"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := c.get(ctx, u, &cm); err == nil {
			d.CommentAuthor, d.CommentBody, d.CommentURL = cm.User.Login, cm.Body, cm.HTMLURL
		}
	}
	return d
}

func (p *Poller) load(account string) (state, bool, error) {
	var st state
	v, ok, err := p.KV.Get(stateGroup, account)
	if err != nil {
		return st, false, fmt.Errorf("reading where %s left off: %w", account, err)
	}
	if !ok {
		return st, true, nil
	}
	if err := json.Unmarshal([]byte(v), &st); err != nil || st.Floor.IsZero() {
		// Unreadable: start again as on first sight rather than from zero,
		// which would publish everything.
		return state{}, true, nil
	}
	return st, false, nil
}

func (p *Poller) save(account string, st state) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := p.KV.Set(stateGroup, account, string(b), 0); err != nil {
		return fmt.Errorf("saving where %s left off: %w", account, err)
	}
	return nil
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Poller) firstWindow() time.Duration {
	if p.FirstWindow > 0 {
		return p.FirstWindow
	}
	return 24 * time.Hour
}

func (p *Poller) perPage() int {
	if p.PerPage > 0 {
		return p.PerPage
	}
	return 50
}
