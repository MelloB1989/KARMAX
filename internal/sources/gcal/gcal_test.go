package gcal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

const acct = "priya@example.com"

var t0 = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// fakeAPI answers with whatever the test put in it and records the queries.
type fakeAPI struct {
	pages   []Page
	errs    []error
	queries []Query
}

func (f *fakeAPI) Events(_ context.Context, account string, q Query) (Page, error) {
	if account != acct {
		return Page{}, errors.New("wrong account " + account)
	}
	f.queries = append(f.queries, q)
	var err error
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	if err != nil {
		return Page{}, err
	}
	if len(f.pages) == 0 {
		return Page{Complete: true}, nil
	}
	p := f.pages[0]
	if len(f.pages) > 1 {
		f.pages = f.pages[1:]
	}
	return p, nil
}

type memStore map[string]State

func (m memStore) Load(a string) (State, bool, error) { st, ok := m[a]; return st, ok, nil }
func (m memStore) Save(a string, st State) error      { m[a] = st; return nil }

func newKV() sources.Seen {
	kv := map[string]string{}
	return sources.Seen{Group: "gcal-test",
		Get: func(g, k string) (string, bool, error) { v, ok := kv[g+"/"+k]; return v, ok, nil },
		Set: func(g, k, v string, _ time.Duration) error { kv[g+"/"+k] = v; return nil }}
}

type rig struct {
	api   *fakeAPI
	store memStore
	tr    *Tracker
	now   time.Time
	got   []sources.Event
}

func newRig() *rig {
	r := &rig{api: &fakeAPI{}, store: memStore{}, now: t0}
	r.tr = New(Config{Location: time.UTC}, r.api, r.store, newKV())
	r.tr.now = func() time.Time { return r.now }
	return r
}

func (r *rig) tick(t *testing.T, pages ...Page) []sources.Event {
	t.Helper()
	r.api.pages = pages
	r.got = nil
	if _, err := r.tr.Tick(context.Background(), acct, func(e sources.Event) error {
		r.got = append(r.got, e)
		return nil
	}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	return r.got
}

func types(evs []sources.Event) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Type)
	}
	return strings.Join(s, ",")
}

func ev(id string, start time.Time, mins int) Event {
	return Event{ID: id, CalendarID: "primary", Status: "confirmed", Summary: "Event " + id,
		Start: start, End: start.Add(time.Duration(mins) * time.Minute),
		Created: t0.Add(-72 * time.Hour), Updated: t0.Add(-48 * time.Hour), Link: "https://calendar.example.test/" + id}
}

func invite(id string, start time.Time) Event {
	e := ev(id, start, 30)
	e.Organizer = "rahul@example.com"
	e.MyResponse = "needsAction"
	e.Attendees = []Attendee{{Email: "rahul@example.com", Organizer: true, Response: "accepted"}, {Email: acct, Self: true, Response: "needsAction"}}
	return e
}

// The first look at an account must not flood: only what happens in the next
// day is told, the rest is remembered as already known.
func TestFirstSightPublishesOnlyTheNextDay(t *testing.T) {
	r := newRig()
	page := Page{Complete: true, Events: []Event{
		ev("soon-ish", t0.Add(3*time.Hour), 30),
		invite("inv-tomorrow", t0.Add(20*time.Hour)),
		ev("next-week", t0.Add(6*24*time.Hour), 60),
		invite("inv-next-week", t0.Add(7*24*time.Hour)),
	}}
	got := r.tick(t, page)
	if types(got) != "event.upcoming,invitation" {
		t.Fatalf("first sight published %q, want the two events of the next 24h", types(got))
	}
	for _, e := range got {
		if e.Source != "gcal" || e.Account != acct || e.Urgent {
			t.Errorf("%s: source %q account %q urgent %v", e.Type, e.Source, e.Account, e.Urgent)
		}
	}
	if !strings.Contains(got[1].Title, "Invitation") || !strings.Contains(got[1].Title, "rahul@example.com") {
		t.Errorf("invitation title %q should say it is one and who sent it", got[1].Title)
	}
	if got[1].Data["event_id"] != "inv-tomorrow" || got[1].Data["calendar_id"] != "primary" {
		t.Errorf("an invitation must carry what acting on it needs: %v", got[1].Data)
	}

	r.now = t0.Add(5 * time.Minute)
	if again := r.tick(t, page); len(again) != 0 {
		t.Fatalf("nothing changed, yet published %q", types(again))
	}
}

func TestNewEventsAndInvitationsAfterFirstSight(t *testing.T) {
	r := newRig()
	r.tick(t, Page{Complete: true})

	r.now = t0.Add(5 * time.Minute)
	fresh := ev("new", t0.Add(48*time.Hour), 30)
	fresh.Created, fresh.Updated = t0.Add(2*time.Minute), t0.Add(2*time.Minute)
	inv := invite("inv", t0.Add(72*time.Hour))
	inv.Created, inv.Updated = t0.Add(3*time.Minute), t0.Add(3*time.Minute)
	got := r.tick(t, Page{Complete: true, Events: []Event{fresh, inv}})
	if types(got) != "event.created,invitation" {
		t.Fatalf("published %q, want event.created,invitation", types(got))
	}
	if got[1].Data["my_response"] != "needsAction" {
		t.Errorf("invitation data: %v", got[1].Data)
	}
}

func TestAMovedEventIsToldWhatChanged(t *testing.T) {
	r := newRig()
	e := ev("standup", t0.Add(48*time.Hour), 30)
	r.tick(t, Page{Complete: true, Events: []Event{e}})

	r.now = t0.Add(5 * time.Minute)
	moved := e
	moved.Start, moved.End = e.Start.Add(time.Hour), e.End.Add(time.Hour)
	moved.Updated = t0.Add(4 * time.Minute)
	got := r.tick(t, Page{Complete: true, Events: []Event{moved}})
	if types(got) != "event.changed" {
		t.Fatalf("published %q, want event.changed", types(got))
	}
	if !strings.Contains(got[0].Summary, "moved") || !strings.Contains(got[0].Title, "10:00") {
		t.Errorf("a move should say from and to: title %q summary %q", got[0].Title, got[0].Summary)
	}

	// The operator's own answer is not news.
	r.now = t0.Add(10 * time.Minute)
	answered := moved
	answered.MyResponse = "accepted"
	answered.Updated = t0.Add(9 * time.Minute)
	if got := r.tick(t, Page{Complete: true, Events: []Event{answered}}); len(got) != 0 {
		t.Fatalf("the operator's own response was published as %q", types(got))
	}
}

func TestAnInvitationAskedAgainIsAnInvitation(t *testing.T) {
	r := newRig()
	e := invite("review", t0.Add(48*time.Hour))
	e.MyResponse = "accepted"
	r.tick(t, Page{Complete: true, Events: []Event{e}})

	r.now = t0.Add(5 * time.Minute)
	again := e
	again.Start, again.End = e.Start.Add(2*time.Hour), e.End.Add(2*time.Hour)
	again.MyResponse = "needsAction"
	again.Updated = t0.Add(4 * time.Minute)
	got := r.tick(t, Page{Complete: true, Events: []Event{again}})
	if types(got) != "invitation" {
		t.Fatalf("published %q, want the re-asked invitation", types(got))
	}
}

func TestCancellations(t *testing.T) {
	r := newRig()
	one := ev("one", t0.Add(30*time.Hour), 30)
	i1 := ev("series_1", t0.Add(50*time.Hour), 30)
	i1.RecurringID = "series"
	i2 := ev("series_2", t0.Add(74*time.Hour), 30)
	i2.RecurringID = "series"
	r.tick(t, Page{Complete: true, Events: []Event{one, i1, i2}})

	r.now = t0.Add(5 * time.Minute)
	// A path that lists instances says each one is cancelled too; one that
	// lists changes says only the series is. Either is told once per event.
	got := r.tick(t, Page{Complete: true, Events: []Event{
		{ID: "one", Status: "cancelled", Updated: t0.Add(4 * time.Minute)},
		{ID: "series", Status: "cancelled", Updated: t0.Add(4 * time.Minute)},
		{ID: "series_1", RecurringID: "series", Status: "cancelled"},
	}})
	if types(got) != "event.cancelled,event.cancelled,event.cancelled" {
		t.Fatalf("published %q, want the event and both instances of the cancelled series", types(got))
	}
	if !strings.Contains(got[0].Title, "Event one") {
		t.Errorf("a cancellation should name what was cancelled, from what was known: %q", got[0].Title)
	}

	// Still listed as cancelled next time: told once.
	r.now = t0.Add(10 * time.Minute)
	if again := r.tick(t, Page{Complete: true, Events: []Event{{ID: "one", Status: "cancelled"}}}); len(again) != 0 {
		t.Fatalf("a cancellation was told twice: %q", types(again))
	}
}

func TestStartingSoonIsUrgentAndToldOnce(t *testing.T) {
	r := newRig()
	e := ev("call", t0.Add(2*time.Hour), 30)
	declined := ev("skip", t0.Add(2*time.Hour), 30)
	declined.MyResponse = "declined"
	allDay := ev("holiday", t0.Add(2*time.Hour), 24*60)
	allDay.AllDay = true
	r.tick(t, Page{Complete: true, Events: []Event{e, declined, allDay}})

	r.now = t0.Add(2*time.Hour - 12*time.Minute)
	got := r.tick(t, Page{Complete: true, Events: []Event{e, declined, allDay}})
	if types(got) != "event.starting_soon" || !got[0].Urgent {
		t.Fatalf("published %q, want one urgent starting-soon for the accepted event", types(got))
	}
	if !strings.Contains(got[0].Title, "12 min") {
		t.Errorf("title %q should say how soon", got[0].Title)
	}
	r.now = r.now.Add(5 * time.Minute)
	if again := r.tick(t, Page{Complete: true, Events: []Event{e, declined, allDay}}); len(again) != 0 {
		t.Fatalf("starting soon told twice: %q", types(again))
	}
}

// As days pass, events already on the calendar enter the watched window. They
// are not new, and saying so would be noise; an unanswered invitation among
// them still is worth telling.
func TestEventsEnteringTheWindowByTimeAreNotNew(t *testing.T) {
	r := newRig()
	r.tick(t, Page{Complete: true})
	r.now = t0.Add(time.Hour)
	old := ev("old", t0.Add(14*24*time.Hour), 30)
	pending := invite("pending", t0.Add(14*24*time.Hour))
	got := r.tick(t, Page{Complete: true, Events: []Event{old, pending}})
	if types(got) != "invitation" {
		t.Fatalf("published %q, want only the unanswered invitation", types(got))
	}
}

func TestSyncTokensAreUsedAndRenewed(t *testing.T) {
	r := newRig()
	r.tick(t, Page{Complete: true, SyncToken: "tok-1"})
	if q := r.api.queries[0]; q.SyncToken != "" || q.From.IsZero() || q.To.Sub(q.From) < 14*24*time.Hour {
		t.Fatalf("the first listing must be the whole window without a token: %+v", q)
	}

	r.now = t0.Add(5 * time.Minute)
	e := ev("added", t0.Add(30*time.Hour), 30)
	e.Created, e.Updated = t0.Add(4*time.Minute), t0.Add(4*time.Minute)
	got := r.tick(t, Page{Events: []Event{e}, SyncToken: "tok-2"})
	if r.api.queries[1].SyncToken != "tok-1" {
		t.Fatalf("second query used token %q, want tok-1", r.api.queries[1].SyncToken)
	}
	if types(got) != "event.created" {
		t.Fatalf("a change from a token page: %q", types(got))
	}

	// A partial page leaves what it did not mention alone.
	r.now = t0.Add(10 * time.Minute)
	if got := r.tick(t, Page{SyncToken: "tok-3"}); len(got) != 0 {
		t.Fatalf("an empty change list published %q", types(got))
	}
	if _, ok := r.store[acct].Events["added"]; !ok {
		t.Fatal("an empty change list dropped a known event")
	}

	// Google forgot the token: list the window again, quietly.
	r.now = t0.Add(15 * time.Minute)
	r.api.errs = []error{ErrSyncExpired}
	got = r.tick(t, Page{Complete: true, Events: []Event{e}, SyncToken: "tok-4"})
	if n := len(r.api.queries); r.api.queries[n-2].SyncToken != "tok-3" || r.api.queries[n-1].SyncToken != "" {
		t.Fatalf("after an expired token the window must be listed afresh: %+v", r.api.queries[n-2:])
	}
	if len(got) != 0 {
		t.Fatalf("a relisting published %q", types(got))
	}

	// The window slides, so the whole of it is listed again now and then.
	r.now = t0.Add(7 * time.Hour)
	r.tick(t, Page{Complete: true, SyncToken: "tok-5"})
	if q := r.api.queries[len(r.api.queries)-1]; q.SyncToken != "" {
		t.Fatalf("after hours on tokens alone the window must be relisted, got token %q", q.SyncToken)
	}
}

func TestACompleteListingForgetsWhatLeftTheWindow(t *testing.T) {
	r := newRig()
	e := ev("gone", t0.Add(30*time.Hour), 30)
	r.tick(t, Page{Complete: true, Events: []Event{e}})
	r.now = t0.Add(5 * time.Minute)
	if got := r.tick(t, Page{Complete: true}); len(got) != 0 {
		t.Fatalf("an event that left the window published %q", types(got))
	}
	if _, ok := r.store[acct].Events["gone"]; ok {
		t.Fatal("an event absent from a complete listing was kept")
	}
}

func TestTheOrganizerHearsWhoDeclined(t *testing.T) {
	r := newRig()
	e := ev("mine", t0.Add(30*time.Hour), 30)
	e.OrganizerSelf = true
	e.Attendees = []Attendee{{Email: acct, Self: true, Organizer: true, Response: "accepted"}, {Email: "rahul@example.com", Name: "Rahul", Response: "needsAction"}}
	r.tick(t, Page{Complete: true, Events: []Event{e}})

	r.now = t0.Add(5 * time.Minute)
	d := e
	d.Attendees = []Attendee{e.Attendees[0], {Email: "rahul@example.com", Name: "Rahul", Response: "declined"}}
	d.Updated = t0.Add(4 * time.Minute)
	got := r.tick(t, Page{Complete: true, Events: []Event{d}})
	if types(got) != "event.changed" || !strings.Contains(got[0].Summary, "Rahul") || !strings.Contains(got[0].Summary, "declined") {
		t.Fatalf("published %q %v, want a change naming who declined", types(got), got)
	}
}

func TestAFailedListingChangesNothing(t *testing.T) {
	r := newRig()
	r.api.errs = []error{errors.New("boom")}
	r.api.pages = nil
	_, err := r.tr.Tick(context.Background(), acct, func(sources.Event) error { return nil })
	if err == nil {
		t.Fatal("a failed listing reported success")
	}
	if _, ok := r.store[acct]; ok {
		t.Fatal("a failed listing saved state, so the account would never get its first look")
	}
}
