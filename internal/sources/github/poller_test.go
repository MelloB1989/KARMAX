package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ghconn "github.com/MelloB1989/karmax/internal/connectors/github"
	"github.com/MelloB1989/karmax/internal/sources"
)

var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// hub is a GitHub serving several accounts, each under /<login>/.
type hub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	notes    map[string][]map[string]any
	lastMod  map[string]string
	tokens   map[string]string
	interval string
	reqs     []hubReq
}

type hubReq struct{ login, path, query, ims string }

func newHub(t *testing.T) *hub {
	h := &hub{t: t, notes: map[string][]map[string]any{}, lastMod: map[string]string{},
		tokens: map[string]string{}, interval: "60"}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hub) base(login string) string { return h.srv.URL + "/" + login }

func (h *hub) account(login string) ghconn.Account {
	return ghconn.Account{Host: "github.com", Login: login, State: "success", API: h.base(login)}
}

// add puts a thread in login's inbox and moves its Last-Modified on.
func (h *hub) add(login, id, reason string, updated time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.base(login)
	n := map[string]any{"id": id, "unread": true, "reason": reason, "updated_at": updated.Format(time.RFC3339),
		"subject": map[string]any{"title": "Thread " + id, "type": "PullRequest",
			"url":                b + "/repos/contoso/app/pulls/" + id,
			"latest_comment_url": b + "/repos/contoso/app/issues/comments/" + id},
		"repository": map[string]any{"full_name": "contoso/app", "html_url": "https://github.com/contoso/app",
			"owner": map[string]any{"login": "contoso"}}}
	kept := h.notes[login][:0]
	for _, old := range h.notes[login] {
		if old["id"] != id {
			kept = append(kept, old)
		}
	}
	h.notes[login] = append(kept, n)
	h.lastMod[login] = updated.Add(time.Second).Format(http.TimeFormat)
}

// clear empties an inbox, as marking everything done on github.com does.
func (h *hub) clear(login string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notes[login] = nil
	h.lastMod[login] = time.Now().Format(http.TimeFormat)
}

func (h *hub) requests(login, path string) []hubReq {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []hubReq
	for _, r := range h.reqs {
		if r.login == login && r.path == path {
			out = append(out, r)
		}
	}
	return out
}

func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		// PATCH /notifications/threads/:id would mark a thread read; that is
		// the operator's inbox, and nothing here may touch it.
		h.t.Errorf("%s %s: the source wrote to GitHub", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	login, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	rest = "/" + rest
	h.mu.Lock()
	h.reqs = append(h.reqs, hubReq{login: login, path: rest, query: r.URL.RawQuery, ims: r.Header.Get("If-Modified-Since")})
	want, notes, lastMod, interval := h.tokens[login], append([]map[string]any(nil), h.notes[login]...), h.lastMod[login], h.interval
	h.mu.Unlock()

	if want == "" || r.Header.Get("Authorization") != "Bearer "+want {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
		return
	}
	switch {
	case rest == "/notifications":
		if r.URL.Query().Get("all") != "true" {
			h.t.Errorf("notifications asked without all=true: %s", r.URL.RawQuery)
		}
		w.Header().Set("X-Poll-Interval", interval)
		w.Header().Set("Last-Modified", lastMod)
		if ims := r.Header.Get("If-Modified-Since"); ims != "" && ims == lastMod {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		sort.Slice(notes, func(i, j int) bool { return notes[i]["updated_at"].(string) > notes[j]["updated_at"].(string) })
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		lo, hi := (page-1)*per, page*per
		if lo > len(notes) {
			lo = len(notes)
		}
		if hi > len(notes) {
			hi = len(notes)
		}
		if hi < len(notes) {
			q := r.URL.Query()
			q.Set("page", strconv.Itoa(page+1))
			w.Header().Set("Link", fmt.Sprintf(`<%s/notifications?%s>; rel="next"`, h.base(login), q.Encode()))
		}
		_ = json.NewEncoder(w).Encode(notes[lo:hi])
	case strings.HasPrefix(rest, "/repos/contoso/app/pulls/"):
		n := strings.TrimPrefix(rest, "/repos/contoso/app/pulls/")
		fmt.Fprintf(w, `{"number":%s,"state":"open","html_url":"https://github.com/contoso/app/pull/%s","user":{"login":"alice"}}`, n, n)
	case strings.HasPrefix(rest, "/repos/contoso/app/issues/comments/"):
		n := strings.TrimPrefix(rest, "/repos/contoso/app/issues/comments/")
		fmt.Fprintf(w, `{"body":"comment on %s","html_url":"https://github.com/contoso/app/pull/%s#c","user":{"login":"bob"}}`, n, n)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type fakeLogins struct {
	mu       sync.Mutex
	accounts []ghconn.Account
	tokens   map[string]string
}

func (f *fakeLogins) set(a ...ghconn.Account) {
	f.mu.Lock()
	f.accounts = a
	f.mu.Unlock()
}

func (f *fakeLogins) Accounts(context.Context) ([]ghconn.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ghconn.Account(nil), f.accounts...), nil
}

func (f *fakeLogins) Token(_ context.Context, a ghconn.Account) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tok, ok := f.tokens[a.Name()]; ok {
		return tok, nil
	}
	return "", errors.New("no oauth token found for " + a.Name())
}

type rig struct {
	hub    *hub
	logins *fakeLogins
	kv     map[string]string
	now    time.Time
	events []sources.Event
	failPublish int
	p      *Poller
}

func newRig(t *testing.T, logins ...string) *rig {
	r := &rig{hub: newHub(t), logins: &fakeLogins{tokens: map[string]string{}}, kv: map[string]string{}, now: t0}
	var accts []ghconn.Account
	for _, l := range logins {
		r.hub.tokens[l] = "tok-" + l
		r.logins.tokens[l] = "tok-" + l
		accts = append(accts, r.hub.account(l))
	}
	r.logins.set(accts...)
	r.p = &Poller{
		Logins: r.logins,
		KV: KV{
			Get: func(g, k string) (string, bool, error) { v, ok := r.kv[g+"/"+k]; return v, ok, nil },
			Set: func(g, k, v string, _ time.Duration) error { r.kv[g+"/"+k] = v; return nil },
		},
		Publish: func(e sources.Event) error {
			if r.failPublish > 0 {
				r.failPublish--
				return errors.New("bus is down")
			}
			r.events = append(r.events, e)
			return nil
		},
		Now: func() time.Time { return r.now },
	}
	return r
}

func (r *rig) tick(t *testing.T) Result {
	t.Helper()
	res, err := r.p.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	return res
}

func (r *rig) take() []sources.Event {
	out := r.events
	r.events = nil
	return out
}

func keys(es []sources.Event) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Account+":"+strings.SplitN(e.Key, "@", 2)[0])
	}
	return out
}

func TestFirstSightPublishesTheLastDayOldestFirstAndNothingOlderEver(t *testing.T) {
	r := newRig(t, "octo-user")
	r.hub.add("octo-user", "1", "mention", t0.Add(-1*time.Hour))
	r.hub.add("octo-user", "2", "review_requested", t0.Add(-2*time.Hour))
	r.hub.add("octo-user", "3", "subscribed", t0.Add(-30*time.Hour))

	r.tick(t)
	got := r.take()
	if fmt.Sprint(keys(got)) != "[octo-user:2 octo-user:1]" {
		t.Fatalf("published %v, want the last day's two, oldest first", keys(got))
	}
	e := got[0]
	if e.Account != "octo-user" || e.Type != "review_requested.PullRequest" || !e.Urgent {
		t.Errorf("event = %+v", e)
	}
	if e.Data["state"] != "open" || e.Data["author"] != "alice" || !strings.Contains(e.Summary, "comment on 2") {
		t.Errorf("the event was not filled in from the subject and its latest comment: %v\n%s", e.Data, e.Summary)
	}
	if e.URL != "https://github.com/contoso/app/pull/2" {
		t.Errorf("url = %q", e.URL)
	}

	// The operator clears their inbox: what was behind the first page moves up.
	// None of it is new.
	r.hub.clear("octo-user")
	r.hub.add("octo-user", "4", "subscribed", t0.Add(-40*time.Hour))
	r.hub.add("octo-user", "3", "subscribed", t0.Add(-30*time.Hour))
	r.now = t0.Add(5 * time.Minute)
	r.tick(t)
	if got := r.take(); len(got) != 0 {
		t.Errorf("old threads were published as new: %v", keys(got))
	}
}

func TestAnUnchangedInboxIsOneConditionalRequest(t *testing.T) {
	r := newRig(t, "octo-user")
	r.hub.add("octo-user", "1", "mention", t0.Add(-time.Hour))
	r.tick(t)
	r.take()

	r.now = t0.Add(3 * time.Minute)
	res := r.tick(t)
	if len(r.take()) != 0 {
		t.Error("an unchanged inbox published again")
	}
	reqs := r.hub.requests("octo-user", "/notifications")
	if len(reqs) != 2 || reqs[1].ims == "" {
		t.Fatalf("requests = %+v, want a second one carrying If-Modified-Since", reqs)
	}
	if len(res.Accounts) != 1 || !res.Accounts[0].NotModified {
		t.Errorf("result = %+v, want the account reported as unchanged", res)
	}
}

func TestGitHubsPollIntervalIsHonoured(t *testing.T) {
	r := newRig(t, "octo-user")
	r.hub.interval = "600"
	r.hub.add("octo-user", "1", "mention", t0.Add(-time.Hour))
	r.tick(t)

	r.now = t0.Add(3 * time.Minute)
	res := r.tick(t)
	if n := len(r.hub.requests("octo-user", "/notifications")); n != 1 {
		t.Errorf("polled %d times inside GitHub's ten-minute interval", n)
	}
	if !res.Accounts[0].Waiting {
		t.Errorf("result = %+v, want the account reported as waiting", res.Accounts[0])
	}

	r.now = t0.Add(11 * time.Minute)
	r.tick(t)
	if n := len(r.hub.requests("octo-user", "/notifications")); n != 2 {
		t.Errorf("polled %d times once the interval had passed, want 2", n)
	}
}

func TestAThreadWithNewActivityIsPublishedAgain(t *testing.T) {
	r := newRig(t, "octo-user")
	r.hub.add("octo-user", "1", "author", t0.Add(-time.Hour))
	r.tick(t)
	first := r.take()

	r.now = t0.Add(5 * time.Minute)
	r.hub.add("octo-user", "1", "author", t0.Add(4*time.Minute))
	r.tick(t)
	again := r.take()
	if len(first) != 1 || len(again) != 1 || first[0].Key == again[0].Key {
		t.Fatalf("first %v then %v, want the thread twice under two keys", first, again)
	}
}

func TestABrokenAccountIsReportedAndNeverHoldsUpTheOthers(t *testing.T) {
	r := newRig(t, "octo-user", "octo-work")
	delete(r.logins.tokens, "octo-work") // gh lost this login's token
	broken := r.hub.account("octo-gone")
	broken.State, broken.Error = "error", "The token in keyring is invalid."
	r.logins.set(append(r.logins.accounts, broken)...)
	r.hub.add("octo-user", "1", "mention", t0.Add(-time.Hour))

	res := r.tick(t)
	if got := keys(r.take()); fmt.Sprint(got) != "[octo-user:1]" {
		t.Errorf("published %v, want octo-user's event despite the others", got)
	}
	failed := map[string]string{}
	for _, a := range res.Failed() {
		failed[a.Account] = a.Err.Error()
	}
	// Each result carries its login, so whoever reads it can check or
	// register that login without listing them again.
	for _, a := range res.Accounts {
		if a.Login.Name() != a.Account || a.Login.APIBase() == "" {
			t.Errorf("result %q carries login %+v", a.Account, a.Login)
		}
	}
	if !strings.Contains(failed["octo-work"], "token") || !strings.Contains(failed["octo-gone"], "invalid") {
		t.Errorf("failures = %v, want both broken accounts named with why", failed)
	}

	// Every account broken is the source not working at all, which is an error.
	r.logins.set(broken)
	if _, err := r.p.Tick(context.Background()); err == nil {
		t.Error("a tick in which no account could be read reported success")
	}
}

func TestEachAccountIsReadWithItsOwnToken(t *testing.T) {
	r := newRig(t, "octo-user", "octo-work")
	r.hub.add("octo-user", "1", "mention", t0.Add(-time.Hour))
	r.hub.add("octo-work", "7", "assign", t0.Add(-time.Hour))
	r.tick(t)
	got := keys(r.take())
	sort.Strings(got)
	if fmt.Sprint(got) != "[octo-user:1 octo-work:7]" {
		t.Errorf("published %v, want one from each account", got)
	}

	// A token GitHub rejects is that account's failure, and the token itself
	// is never in the report.
	r.hub.tokens["octo-work"] = "rotated"
	r.hub.add("octo-work", "8", "assign", t0.Add(time.Minute))
	r.now = t0.Add(5 * time.Minute)
	res := r.tick(t)
	for _, a := range res.Failed() {
		if strings.Contains(a.Err.Error(), "tok-octo-work") {
			t.Errorf("the error carries the token: %v", a.Err)
		}
	}
	if len(res.Failed()) != 1 || res.Failed()[0].Account != "octo-work" {
		t.Errorf("failed = %+v, want octo-work alone", res.Failed())
	}
}

func TestALoginAddedWhileRunningIsPolledNextTick(t *testing.T) {
	r := newRig(t, "octo-user")
	r.hub.add("octo-work", "9", "review_requested", t0.Add(-time.Hour))
	r.tick(t)
	r.take()

	// `gh auth login` as octo-work, no restart.
	r.hub.tokens["octo-work"] = "tok-octo-work"
	r.logins.tokens["octo-work"] = "tok-octo-work"
	r.logins.set(r.hub.account("octo-user"), r.hub.account("octo-work"))
	r.now = t0.Add(3 * time.Minute)
	r.tick(t)
	if got := keys(r.take()); fmt.Sprint(got) != "[octo-work:9]" {
		t.Errorf("published %v, want the new login's recent thread", got)
	}
}

func TestAnEventThatCouldNotBePublishedIsTriedAgain(t *testing.T) {
	r := newRig(t, "octo-user")
	r.hub.add("octo-user", "1", "mention", t0.Add(-time.Hour))
	r.failPublish = 1
	res, _ := r.p.Tick(context.Background())
	if len(res.Failed()) != 1 {
		t.Errorf("a failed publish was not reported: %+v", res)
	}

	r.now = t0.Add(3 * time.Minute)
	r.tick(t)
	if got := keys(r.take()); fmt.Sprint(got) != "[octo-user:1]" {
		t.Errorf("published %v on the retry, want the event exactly once", got)
	}
}

func TestOlderPagesAreReadOnlyWhileTheyStillHoldNewThreads(t *testing.T) {
	r := newRig(t, "octo-user")
	r.p.PerPage = 2
	for i, age := range []time.Duration{1, 2, 3, 30, 40, 50} {
		r.hub.add("octo-user", strconv.Itoa(i+1), "subscribed", t0.Add(-age*time.Hour))
	}
	r.tick(t)
	if got := keys(r.take()); fmt.Sprint(got) != "[octo-user:3 octo-user:2 octo-user:1]" {
		t.Errorf("published %v, want the three from the last day", got)
	}
	pages := r.hub.requests("octo-user", "/notifications")
	if len(pages) != 2 {
		t.Errorf("read %d pages, want 2: the second already reaches past the last day", len(pages))
	}
}

// A notification names its subject by URL. A token goes only to the account's
// own API, never to a host a payload pointed at.
func TestATokenIsNeverSentToAnotherHost(t *testing.T) {
	other := 0
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other++
		if r.Header.Get("Authorization") != "" {
			t.Errorf("a token was sent to %s", r.URL)
		}
	}))
	defer elsewhere.Close()

	r := newRig(t, "octo-user")
	r.hub.add("octo-user", "1", "mention", t0.Add(-time.Hour))
	r.hub.mu.Lock()
	subj := r.hub.notes["octo-user"][0]["subject"].(map[string]any)
	subj["url"] = elsewhere.URL + "/repos/contoso/app/pulls/1"
	subj["latest_comment_url"] = elsewhere.URL + "/repos/contoso/app/issues/comments/1"
	r.hub.mu.Unlock()

	r.tick(t)
	if got := r.take(); len(got) != 1 {
		t.Fatalf("published %d events, want the thread even unread", len(got))
	}
	if other != 0 {
		t.Errorf("%d requests went to a host that is not the account's", other)
	}
}
