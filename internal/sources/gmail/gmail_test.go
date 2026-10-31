package gmail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

const acct = "priya@example.com"

var t0 = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

type fakeAPI struct {
	start      string
	startErr   error
	history    map[string]Page // by start
	historyErr error
	recent     []Ref // newest first
	msgs       map[string]Message
	msgErr     map[string]error
	threads    map[string][]Ref
	calls      map[string]int
}

func newFake() *fakeAPI {
	return &fakeAPI{start: "100", history: map[string]Page{}, msgs: map[string]Message{},
		msgErr: map[string]error{}, threads: map[string][]Ref{}, calls: map[string]int{}}
}

func (f *fakeAPI) Start(context.Context, string) (string, error) {
	f.calls["start"]++
	return f.start, f.startErr
}

func (f *fakeAPI) History(_ context.Context, _ string, start string) (Page, error) {
	f.calls["history"]++
	if f.historyErr != nil {
		return Page{}, f.historyErr
	}
	p, ok := f.history[start]
	if !ok {
		return Page{Next: start}, nil
	}
	return p, nil
}

func (f *fakeAPI) Recent(_ context.Context, _ string, max int) ([]Ref, error) {
	f.calls["recent"]++
	if len(f.recent) > max {
		return f.recent[:max], nil
	}
	return f.recent, nil
}

func (f *fakeAPI) Message(_ context.Context, _ string, id string) (Message, error) {
	f.calls["message:"+id]++
	if err := f.msgErr[id]; err != nil {
		return Message{}, err
	}
	m, ok := f.msgs[id]
	if !ok {
		return Message{}, ErrGone
	}
	return m, nil
}

func (f *fakeAPI) Thread(_ context.Context, _ string, id string) ([]Ref, error) {
	f.calls["thread:"+id]++
	return f.threads[id], nil
}

type memStore map[string]State

func (m memStore) Load(a string) (State, bool, error) { st, ok := m[a]; return st, ok, nil }
func (m memStore) Save(a string, st State) error      { m[a] = st; return nil }

func newSeen() sources.Seen {
	kv := map[string]string{}
	return sources.Seen{Group: "gmail-test",
		Get: func(g, k string) (string, bool, error) { v, ok := kv[g+"/"+k]; return v, ok, nil },
		Set: func(g, k, v string, _ time.Duration) error { kv[g+"/"+k] = v; return nil }}
}

type rig struct {
	api   *fakeAPI
	store memStore
	w     *Watcher
	now   time.Time
}

func newRig(cfg Config) *rig {
	r := &rig{api: newFake(), store: memStore{}, now: t0}
	cfg.Location = time.UTC
	r.w = New(cfg, r.api, r.store, newSeen())
	r.w.now = func() time.Time { return r.now }
	return r
}

func (r *rig) tick(t *testing.T) ([]sources.Event, Result) {
	t.Helper()
	var got []sources.Event
	res, err := r.w.Tick(context.Background(), acct, func(e sources.Event) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	return got, res
}

// watching is a rig past its first look, at history id "100".
func watching(cfg Config) *rig {
	r := newRig(cfg)
	r.store[acct] = State{FirstSeen: t0.Add(-time.Hour), HistoryID: "100", LastTick: t0.Add(-2 * time.Minute)}
	return r
}

// mail is a message from a person to the operator, in the inbox.
func mail(id string, at time.Time, labels ...string) Message {
	return Message{
		ID: id, ThreadID: id, At: at, Snippet: "Can you send the signed copy by Friday? Thanks &amp; regards",
		Labels: append([]string{"INBOX", "UNREAD"}, labels...),
		Headers: []Header{
			{"From", "Dana Reyes <dana@example.org>"},
			{"To", "Priya Sharma <priya@example.com>"},
			{"Subject", "Lease renewal"},
			{"Message-ID", "<" + id + "@mail.example.org>"},
		},
		Attachments: []string{"lease.pdf"},
	}
}

func (r *rig) arrive(m Message) {
	r.api.msgs[m.ID] = m
	p := r.api.history["100"]
	p.Added = append(p.Added, Ref{ID: m.ID, ThreadID: m.ThreadID, Labels: m.Labels})
	p.Next = "200"
	r.api.history["100"] = p
}

func with(m Message, name, value string) Message {
	out := m
	out.Headers = nil
	replaced := false
	for _, h := range m.Headers {
		if strings.EqualFold(h.Name, name) {
			out.Headers = append(out.Headers, Header{name, value})
			replaced = true
			continue
		}
		out.Headers = append(out.Headers, h)
	}
	if !replaced {
		out.Headers = append(out.Headers, Header{name, value})
	}
	return out
}

// The first look at an account brings what is still unread from the last day
// as backlog: context, never a page, however important.
func TestTheFirstLookIsUnreadBacklogAndNeverPages(t *testing.T) {
	r := newRig(Config{})
	fresh := mail("m1", t0.Add(-2*time.Hour), "IMPORTANT")
	read := mail("m2", t0.Add(-3*time.Hour))
	read.Labels = []string{"INBOX"}
	old := mail("m3", t0.Add(-30*time.Hour))
	for _, m := range []Message{fresh, read, old} {
		r.api.msgs[m.ID] = m
	}
	r.api.recent = []Ref{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}}

	got, res := r.tick(t)
	if !res.First || len(got) != 1 || got[0].Key != "m1" {
		t.Fatalf("first look published %+v (%+v), want only the unread message from today", got, res)
	}
	if !got[0].Backlog || got[0].Urgent {
		t.Errorf("a first-look message must be backlog and not urgent: %+v", got[0])
	}
	if st := r.store[acct]; st.HistoryID != "100" || !st.FirstSeen.Equal(t0) {
		t.Errorf("state after the first look %+v", st)
	}
	if r.api.calls["start"] != 1 || r.api.calls["history"] != 0 {
		t.Errorf("calls %v", r.api.calls)
	}
}

func TestANewMessageIsAnEventAboutItsSender(t *testing.T) {
	r := watching(Config{})
	r.arrive(mail("m1", t0.Add(-time.Minute)))
	got, res := r.tick(t)
	if len(got) != 1 || res.Published != 1 {
		t.Fatalf("published %+v", got)
	}
	e := got[0]
	if e.Source != "gmail" || e.Account != acct || e.Type != "message" || e.Key != "m1" || e.Backlog || e.Urgent {
		t.Fatalf("event %+v", e)
	}
	if !strings.Contains(e.Title, "Dana Reyes") || !strings.Contains(e.Title, "Lease renewal") {
		t.Errorf("title %q should name the sender and subject", e.Title)
	}
	if e.Summary != "Can you send the signed copy by Friday? Thanks & regards" {
		t.Errorf("summary %q should be Gmail's snippet, unescaped", e.Summary)
	}
	if !strings.Contains(e.URL, "m1") || !strings.Contains(e.URL, "authuser=priya%40example.com") {
		t.Errorf("url %q", e.URL)
	}
	for k, want := range map[string]any{
		"thread": "m1", "message": "m1", "from": "Dana Reyes <dana@example.org>", "from_address": "dana@example.org",
		"email_subject": "Lease renewal", "addressed_directly": true, "important": false, "operator_in_thread": false,
	} {
		if fmt.Sprint(e.Data[k]) != fmt.Sprint(want) {
			t.Errorf("data[%s] = %v, want %v", k, e.Data[k], want)
		}
	}
	if fmt.Sprint(e.Data["attachments"]) != "[lease.pdf]" || fmt.Sprint(e.Data["to"]) != "[Priya Sharma <priya@example.com>]" {
		t.Errorf("attachments %v to %v", e.Data["attachments"], e.Data["to"])
	}
	// Long-term memory files it under the person, with the people.
	if e.Subject() != "Dana Reyes" || e.Category() != "people" {
		t.Errorf("filed under %q as %q", e.Subject(), e.Category())
	}
	if st := r.store[acct]; st.HistoryID != "200" {
		t.Errorf("history id not advanced: %+v", st)
	}
	if r.api.calls["thread:m1"] != 0 {
		t.Error("the first message of a thread needs no thread lookup")
	}
	if again, _ := r.tick(t); len(again) != 0 {
		t.Errorf("told twice: %+v", again)
	}
}

// Urgent: a person writing to the operator directly, and Gmail marks it
// important or it answers a thread the operator wrote in.
func TestUrgencyIsAPersonWritingDirectlyAboutSomethingThatMatters(t *testing.T) {
	r := watching(Config{})
	important := mail("m1", t0, "IMPORTANT")
	plain := mail("m2", t0)
	cc := with(mail("m3", t0, "IMPORTANT"), "To", "team@example.org")
	cc = with(cc, "Cc", "priya@example.com")
	list := with(mail("m4", t0, "IMPORTANT"), "To", "team@example.org") // reached her through a group
	auto := with(mail("m5", t0, "IMPORTANT"), "Auto-Submitted", "auto-replied")
	for _, m := range []Message{important, plain, cc, list, auto} {
		r.arrive(m)
	}
	got, _ := r.tick(t)
	urgent := map[string]bool{}
	for _, e := range got {
		urgent[e.Key] = e.Urgent
	}
	want := map[string]bool{"m1": true, "m2": false, "m3": true, "m4": false, "m5": false}
	if fmt.Sprint(urgent) != fmt.Sprint(want) {
		t.Fatalf("urgent %v, want %v", urgent, want)
	}
	for _, e := range got {
		if e.Key == "m5" && e.Data["automated"] != true {
			t.Errorf("an auto-reply should say it is automated: %+v", e.Data)
		}
	}
}

// A message in a thread the operator wrote in is a reply, and urgent from a
// person writing to them even when Gmail does not mark it important.
func TestAReplyInTheOperatorsThreadIsAReply(t *testing.T) {
	r := watching(Config{})
	m := mail("m9", t0)
	m.ThreadID = "t1"
	r.arrive(m)
	r.api.threads["t1"] = []Ref{{ID: "t1", Labels: []string{"SENT"}}, {ID: "m9", Labels: []string{"INBOX"}}}
	other := mail("m8", t0)
	other.ThreadID = "t2"
	r.arrive(other)
	r.api.threads["t2"] = []Ref{{ID: "t2", Labels: []string{"INBOX"}}, {ID: "m8", Labels: []string{"INBOX"}}}

	got, _ := r.tick(t)
	byKey := map[string]sources.Event{}
	for _, e := range got {
		byKey[e.Key] = e
	}
	if e := byKey["m9"]; e.Type != "reply" || !e.Urgent || e.Data["operator_in_thread"] != true {
		t.Errorf("reply in her thread: %+v", e)
	}
	if e := byKey["m8"]; e.Type != "message" || e.Urgent {
		t.Errorf("a thread she never wrote in: %+v", e)
	}
}

// Only new inbound mail in the inbox is told: not what the operator sent,
// not drafts, spam or trash, not bulk mail by Gmail's own signals.
func TestOnlyInboundPersonalMailInTheInboxIsTold(t *testing.T) {
	r := watching(Config{Own: func() []string { return []string{"priya@example.net"} }})
	keep := mail("keep", t0)
	sent := mail("sent", t0, "SENT")
	fromOther := with(mail("other", t0), "From", "Priya <priya@example.net>")
	fromSelf := with(mail("self", t0), "From", "priya@example.com")
	promo := mail("promo", t0, "CATEGORY_PROMOTIONS")
	updates := mail("updates", t0, "CATEGORY_UPDATES")
	unsub := with(mail("unsub", t0), "List-Unsubscribe", "<mailto:leave@example.org>")
	bulk := with(mail("bulk", t0), "Precedence", "bulk")
	noreply := with(mail("noreply", t0), "From", "Billing <no-reply@example.org>")
	donot := with(mail("donot", t0), "From", "do_not_reply@example.org")
	archived := mail("archived", t0)
	archived.Labels = []string{"UNREAD"}
	for _, m := range []Message{keep, sent, fromOther, fromSelf, promo, updates, unsub, bulk, noreply, donot, archived} {
		r.arrive(m)
	}
	draft := Ref{ID: "draft", Labels: []string{"DRAFT"}}
	spam := Ref{ID: "spam", Labels: []string{"SPAM", "INBOX"}}
	p := r.api.history["100"]
	p.Added = append(p.Added, draft, spam)
	r.api.history["100"] = p

	got, res := r.tick(t)
	var keys []string
	for _, e := range got {
		keys = append(keys, e.Key)
	}
	if strings.Join(keys, ",") != "keep" {
		t.Fatalf("told %v, want only keep (skipped %v)", keys, res.Skipped)
	}
	for _, id := range []string{"draft", "spam", "sent", "promo"} {
		if r.api.calls["message:"+id] != 0 {
			t.Errorf("%s was fetched although its labels already ruled it out", id)
		}
	}
	if res.Skipped["bulk"] != 4 || res.Skipped["own"] != 3 || res.Skipped["category"] != 2 {
		t.Errorf("skipped %v", res.Skipped)
	}
}

// The categories skipped and the urgency rules are the operator's to set.
func TestCategoriesAndUrgencyAreConfigurable(t *testing.T) {
	r := watching(Config{SkipCategories: []string{}, UrgentWhen: []string{"direct"}})
	r.arrive(mail("promo", t0, "CATEGORY_PROMOTIONS"))
	r.arrive(mail("plain", t0))
	got, _ := r.tick(t)
	if len(got) != 2 {
		t.Fatalf("with no category skipped, both are told: %+v", got)
	}
	for _, e := range got {
		if !e.Urgent {
			t.Errorf("with urgent-when direct, a person writing directly is urgent: %+v", e)
		}
	}

	never := watching(Config{UrgentWhen: []string{}})
	never.arrive(mail("m1", t0, "IMPORTANT"))
	if got, _ := never.tick(t); len(got) != 1 || got[0].Urgent {
		t.Fatalf("with no urgency rule nothing is urgent: %+v", got)
	}

	social := watching(Config{SkipCategories: []string{"social"}})
	social.arrive(mail("promo", t0, "CATEGORY_PROMOTIONS"))
	social.arrive(mail("soc", t0, "CATEGORY_SOCIAL"))
	if got, _ := social.tick(t); len(got) != 1 || got[0].Key != "promo" {
		t.Fatalf("skipping only social: %+v", got)
	}
}

// When Google no longer has the history id, the newest inbox mail since the
// last look is read instead, and watching starts again from now.
func TestAnExpiredHistoryIDFallsBackToTheNewestInboxMail(t *testing.T) {
	r := watching(Config{})
	r.api.historyErr = ErrHistoryExpired
	r.api.start = "500"
	since := mail("new", t0.Add(-time.Minute))
	before := mail("old", t0.Add(-3*time.Hour))
	r.api.msgs["new"], r.api.msgs["old"] = since, before
	r.api.recent = []Ref{{ID: "new"}, {ID: "old"}}

	got, res := r.tick(t)
	if !res.Expired || len(got) != 1 || got[0].Key != "new" || got[0].Backlog {
		t.Fatalf("fallback published %+v (%+v)", got, res)
	}
	if st := r.store[acct]; st.HistoryID != "500" {
		t.Errorf("watching should start again from the current history id: %+v", st)
	}
}

// A message that cannot be read keeps the history id, so the next tick reads
// the same history again; what was already told is not told twice.
func TestAFailedReadIsRetriedWithoutTellingTwice(t *testing.T) {
	r := watching(Config{})
	r.arrive(mail("a", t0))
	r.arrive(mail("b", t0))
	r.api.msgErr["b"] = errors.New("backend error")
	got, res := r.tick(t)
	if len(res.Failed) != 1 || res.Failed[0] != "b" {
		t.Fatalf("failed %v", res.Failed)
	}
	if st := r.store[acct]; st.HistoryID != "100" {
		t.Fatalf("history id moved past an unread message: %+v", st)
	}
	delete(r.api.msgErr, "b")
	again, _ := r.tick(t)
	if len(got) != 1 || len(again) != 1 || got[0].Key != "a" || again[0].Key != "b" {
		t.Fatalf("first %+v then %+v", got, again)
	}
	if r.api.calls["message:a"] != 1 {
		t.Errorf("an already-told message was fetched again: %d", r.api.calls["message:a"])
	}
	if st := r.store[acct]; st.HistoryID != "200" || len(st.Failing) != 0 {
		t.Errorf("state after the retry %+v", st)
	}
}

// A message that never reads is given up after a few ticks, so one bad
// message cannot hold the account's mail back for good.
func TestAMessageThatNeverReadsIsGivenUp(t *testing.T) {
	r := watching(Config{})
	r.arrive(mail("bad", t0))
	r.api.msgErr["bad"] = errors.New("backend error")
	for i := 0; i < maxAttempts; i++ {
		r.tick(t)
	}
	if st := r.store[acct]; st.HistoryID != "200" || len(st.Failing) != 0 {
		t.Fatalf("state after giving up %+v", st)
	}
}

// When the history itself cannot be listed, the tick fails and nothing moves.
func TestAHistoryThatCannotBeListedFailsTheTick(t *testing.T) {
	r := watching(Config{})
	r.api.historyErr = errors.New("signed out")
	if _, err := r.w.Tick(context.Background(), acct, func(sources.Event) error { return nil }); err == nil {
		t.Fatal("want the error")
	}
	if st := r.store[acct]; st.HistoryID != "100" {
		t.Fatalf("state %+v", st)
	}
}

// A message deleted before it could be read is passed over.
func TestAMessageGoneBeforeItIsReadIsPassedOver(t *testing.T) {
	r := watching(Config{})
	p := Page{Added: []Ref{{ID: "gone", Labels: []string{"INBOX"}}}, Next: "200"}
	r.api.history["100"] = p
	got, _ := r.tick(t)
	if len(got) != 0 || r.store[acct].HistoryID != "200" {
		t.Fatalf("got %+v state %+v", got, r.store[acct])
	}
}

// A burst bigger than one tick's reads is worked through over several ticks.
func TestABurstIsReadOverSeveralTicks(t *testing.T) {
	r := watching(Config{MaxPerTick: 2})
	for i := 0; i < 5; i++ {
		r.arrive(mail(fmt.Sprintf("m%d", i), t0))
	}
	total := 0
	for i := 0; i < 3; i++ {
		got, res := r.tick(t)
		total += len(got)
		if i < 2 && (!res.More || r.store[acct].HistoryID != "100") {
			t.Fatalf("tick %d: more %v state %+v", i, res.More, r.store[acct])
		}
	}
	if total != 5 || r.store[acct].HistoryID != "200" {
		t.Fatalf("told %d of 5, state %+v", total, r.store[acct])
	}
}

// A message delivered late — dated before KARMAX started watching — is
// backlog even when it arrives through history.
func TestMailDatedBeforeTheFirstLookIsBacklog(t *testing.T) {
	r := watching(Config{})
	r.arrive(mail("late", t0.Add(-2*time.Hour), "IMPORTANT"))
	got, _ := r.tick(t)
	if len(got) != 1 || !got[0].Backlog || got[0].Urgent {
		t.Fatalf("got %+v", got)
	}
}
