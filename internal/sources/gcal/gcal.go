// Package gcal turns what happens on one Google account's calendar into
// sources.Event: new, changed and cancelled events, invitations waiting for an
// answer, and a reminder shortly before an event starts.
//
// It is pure logic. How events are fetched (a CLI, an API, a sync token or a
// window listing) is the API seam; where the snapshot lives is the Store seam;
// what was already told is sources.Seen.
//
// The snapshot is the centre: every listing is set beside what was known
// before, so a change can say what changed, a cancellation can name what was
// cancelled, and "starting soon" needs no further call.
package gcal

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

// Event is one occurrence on a calendar. Recurring events arrive expanded, one
// Event per occurrence, with RecurringID naming the series.
type Event struct {
	ID          string    `json:"id"`
	CalendarID  string    `json:"cal,omitempty"`
	Status      string    `json:"status,omitempty"` // confirmed, tentative, cancelled
	Summary     string    `json:"summary,omitempty"`
	Description string    `json:"desc,omitempty"`
	Location    string    `json:"loc,omitempty"`
	Link        string    `json:"link,omitempty"`
	Meet        string    `json:"meet,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"allday,omitempty"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	// Organizer is the organizer's address; OrganizerSelf is true when that is
	// the account itself.
	Organizer     string `json:"org,omitempty"`
	OrganizerSelf bool   `json:"orgself,omitempty"`
	// MyResponse is the account's own answer when it is invited: needsAction,
	// accepted, declined or tentative. Empty when it is not an attendee.
	MyResponse  string     `json:"mine,omitempty"`
	Attendees   []Attendee `json:"att,omitempty"`
	RecurringID string     `json:"rec,omitempty"`
	// Type is Google's eventType: default, focusTime, outOfOffice,
	// workingLocation, birthday, fromGmail.
	Type string `json:"type,omitempty"`
}

// Attendee is one guest and their answer.
type Attendee struct {
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
	Response  string `json:"resp,omitempty"`
	Self      bool   `json:"self,omitempty"`
	Organizer bool   `json:"org,omitempty"`
}

// Query is one listing request.
type Query struct {
	From, To time.Time
	// SyncToken asks for what changed since the listing that returned it. Empty
	// asks for the whole window.
	SyncToken string
	// Since is when the previous listing was taken, for a path without sync
	// tokens that can still ask for changes since a time.
	Since time.Time
}

// Page is what a listing returned.
type Page struct {
	Events []Event
	// Complete means Events is every live event in the window, so a known
	// event that is absent has left it. A sync-token page is not complete: it
	// lists only what changed.
	Complete bool
	// SyncToken is for the next query; empty when the path has none.
	SyncToken string
}

// ErrSyncExpired is returned when Google no longer accepts a sync token; the
// window is then listed afresh.
var ErrSyncExpired = errors.New("gcal: the sync token has expired")

// API lists one account's calendar.
type API interface {
	Events(ctx context.Context, account string, q Query) (Page, error)
}

// State is what is remembered about one account between ticks.
type State struct {
	SyncToken string           `json:"sync,omitempty"`
	LastPoll  time.Time        `json:"last_poll"`
	LastFull  time.Time        `json:"last_full"`
	FirstSeen time.Time        `json:"first_seen"`
	Events    map[string]Event `json:"events"`
}

// Store keeps State per account.
type Store interface {
	Load(account string) (State, bool, error)
	Save(account string, st State) error
}

// Config tunes the tracker. Zero values take the defaults.
type Config struct {
	// Location is where times are shown; the operator's own zone.
	Location *time.Location
	// Back and Ahead bound the watched window around now (1 day, 14 days).
	Back, Ahead time.Duration
	// Soon is how close to its start an event is reminded (15 minutes).
	Soon time.Duration
	// FirstLook is how far ahead an account's first look tells (24 hours).
	FirstLook time.Duration
	// FullEvery is how often a sync-token path lists the whole window again,
	// because events already on the calendar slide into the window as days
	// pass and a token only reports changes (6 hours).
	FullEvery time.Duration
}

func (c Config) withDefaults() Config {
	if c.Location == nil {
		c.Location = time.UTC
	}
	if c.Back <= 0 {
		c.Back = 24 * time.Hour
	}
	if c.Ahead <= 0 {
		c.Ahead = 14 * 24 * time.Hour
	}
	if c.Soon <= 0 {
		c.Soon = 15 * time.Minute
	}
	if c.FirstLook <= 0 {
		c.FirstLook = 24 * time.Hour
	}
	if c.FullEvery <= 0 {
		c.FullEvery = 6 * time.Hour
	}
	return c
}

// Tracker watches calendars.
type Tracker struct {
	cfg   Config
	api   API
	store Store
	seen  sources.Seen
	now   func() time.Time
}

// New builds a tracker.
func New(cfg Config, api API, store Store, seen sources.Seen) *Tracker {
	return &Tracker{cfg: cfg.withDefaults(), api: api, store: store, seen: seen, now: time.Now}
}

// Result says what one tick did.
type Result struct {
	First     bool // the account's first look
	Listed    int  // events the listing returned
	Known     int  // events in the snapshot afterwards
	Published int
}

// clockSlack absorbs the difference between this host's clock and Google's
// when deciding whether something changed since the last listing.
const clockSlack = 2 * time.Minute

// Tick lists one account's calendar, publishes what is new, and remembers it.
//
// A failed listing changes nothing, so the next tick tries again from the same
// place. Publishing goes through Seen, so a tick repeated after a crash does
// not tell anything twice.
func (t *Tracker) Tick(ctx context.Context, account string, publish func(sources.Event) error) (Result, error) {
	now := t.now()
	st, found, err := t.store.Load(account)
	if err != nil {
		return Result{}, fmt.Errorf("gcal %s: load state: %w", account, err)
	}
	first := !found || st.FirstSeen.IsZero()
	if st.Events == nil {
		st.Events = map[string]Event{}
	}

	q := Query{From: now.Add(-t.cfg.Back), To: now.Add(t.cfg.Ahead), Since: st.LastPoll}
	if !first && st.SyncToken != "" && now.Sub(st.LastFull) < t.cfg.FullEvery {
		q.SyncToken = st.SyncToken
	}
	page, err := t.api.Events(ctx, account, q)
	if errors.Is(err, ErrSyncExpired) && q.SyncToken != "" {
		q.SyncToken = ""
		page, err = t.api.Events(ctx, account, q)
	}
	if err != nil {
		return Result{}, err
	}

	var out []sources.Event
	emit := func(e sources.Event) { out = append(out, e) }
	since := st.LastPoll.Add(-clockSlack)
	inWindow := func(e Event) bool { return e.End.After(q.From) && e.Start.Before(q.To) }
	listed := map[string]bool{}

	for _, e := range page.Events {
		listed[e.ID] = true
		prev, had := st.Events[e.ID]

		if e.Status == "cancelled" {
			// A cancelled series cancels every occurrence known of it.
			var gone []Event
			if had {
				gone = append(gone, prev)
			}
			for id, k := range st.Events {
				if k.RecurringID != "" && k.RecurringID == e.ID && id != e.ID {
					gone = append(gone, k)
				}
			}
			sort.Slice(gone, func(i, j int) bool { return gone[i].Start.Before(gone[j].Start) })
			for _, g := range gone {
				delete(st.Events, g.ID)
				if !first && g.End.After(now) {
					emit(t.cancelled(account, g, e.Updated))
				}
			}
			continue
		}

		if !inWindow(e) {
			if had {
				// Moved out of the watched window, which is still a change
				// worth hearing about while the event was coming up.
				delete(st.Events, e.ID)
				if !first {
					if what := t.changes(prev, e); len(what) > 0 {
						emit(t.changed(account, prev, e, what))
					}
				}
			}
			continue
		}

		st.Events[e.ID] = e
		switch {
		case first:
			if e.Start.After(now) && e.Start.Before(now.Add(t.cfg.FirstLook)) && !t.soon(e, now) && e.MyResponse != "declined" && !quiet(e) {
				if e.MyResponse == "needsAction" {
					emit(t.invitation(account, e, nil))
				} else {
					emit(t.upcoming(account, e))
				}
			}
		case !had:
			// Not in the snapshot. Either it changed since the last listing,
			// or it was already on the calendar and the window reached it.
			changedSince := e.Updated.After(since)
			switch {
			case e.MyResponse == "needsAction":
				emit(t.invitation(account, e, nil))
			case !changedSince || quiet(e):
			case e.Created.After(since):
				emit(t.created(account, e))
			default:
				emit(t.changed(account, Event{}, e, []string{"now in the next two weeks"}))
			}
		default:
			what := t.changes(prev, e)
			if e.MyResponse == "needsAction" && prev.MyResponse != "needsAction" {
				emit(t.invitation(account, e, what))
			} else if len(what) > 0 && !quiet(e) {
				emit(t.changed(account, prev, e, what))
			}
		}
	}

	if page.Complete {
		for id := range st.Events {
			if !listed[id] {
				delete(st.Events, id) // left the window, or removed without trace
			}
		}
	}
	for id, e := range st.Events {
		if !e.End.After(q.From) {
			delete(st.Events, id)
		}
	}

	upcoming := make([]Event, 0, len(st.Events))
	for _, e := range st.Events {
		if t.soon(e, now) {
			upcoming = append(upcoming, e)
		}
	}
	sort.Slice(upcoming, func(i, j int) bool { return upcoming[i].Start.Before(upcoming[j].Start) })
	for _, e := range upcoming {
		emit(t.startingSoon(account, e, now))
	}

	res := Result{First: first, Listed: len(page.Events), Known: len(st.Events)}
	for _, e := range out {
		isNew, err := t.seen.First(e.Key)
		if err != nil {
			return res, fmt.Errorf("gcal %s: seen: %w", account, err)
		}
		if !isNew {
			continue
		}
		if err := publish(e); err != nil {
			return res, fmt.Errorf("gcal %s: publish: %w", account, err)
		}
		res.Published++
	}

	if page.Complete {
		st.LastFull = now
	}
	st.SyncToken = page.SyncToken
	st.LastPoll = now
	if first {
		st.FirstSeen = now
	}
	if err := t.store.Save(account, st); err != nil {
		return res, fmt.Errorf("gcal %s: save state: %w", account, err)
	}
	return res, nil
}

// soon reports whether e is a meeting starting within the reminder window.
func (t *Tracker) soon(e Event, now time.Time) bool {
	if e.AllDay || e.Status == "cancelled" || e.MyResponse == "declined" || quiet(e) {
		return false
	}
	return e.Start.After(now) && !e.Start.After(now.Add(t.cfg.Soon))
}

// quiet events are the operator's own markers rather than meetings: telling
// them as news, or reminding of them, is noise.
func quiet(e Event) bool {
	switch e.Type {
	case "workingLocation", "outOfOffice", "focusTime", "birthday":
		return true
	}
	return false
}

// changes lists what a person would notice between two versions of an event.
// The account's own response is not among them: that was the operator, or
// KARMAX for them.
func (t *Tracker) changes(prev, cur Event) []string {
	var what []string
	if !prev.Start.Equal(cur.Start) || !prev.End.Equal(cur.End) {
		what = append(what, fmt.Sprintf("moved from %s to %s", t.when(prev), t.when(cur)))
	}
	if prev.Summary != cur.Summary && prev.Summary != "" {
		what = append(what, fmt.Sprintf("renamed from %q", prev.Summary))
	}
	if prev.Location != cur.Location {
		if cur.Location == "" {
			what = append(what, "location removed")
		} else {
			what = append(what, "location now "+cur.Location)
		}
	}
	if prev.Status != cur.Status && cur.Status != "" && prev.Status != "" {
		what = append(what, "now "+cur.Status)
	}
	if cur.OrganizerSelf {
		was := map[string]string{}
		for _, a := range prev.Attendees {
			was[strings.ToLower(a.Email)] = a.Response
		}
		for _, a := range cur.Attendees {
			if a.Self || a.Response == "" || a.Response == "needsAction" {
				continue
			}
			if was[strings.ToLower(a.Email)] != a.Response {
				what = append(what, fmt.Sprintf("%s %s", who(a), a.Response))
			}
		}
	}
	return what
}

func who(a Attendee) string {
	if a.Name != "" && a.Email != "" {
		return a.Name + " <" + a.Email + ">"
	}
	if a.Name != "" {
		return a.Name
	}
	return a.Email
}

// when renders an event's time in the operator's zone.
func (t *Tracker) when(e Event) string {
	if e.Start.IsZero() {
		return "an unknown time"
	}
	s := e.Start.In(t.cfg.Location)
	if e.AllDay {
		return s.Format("Mon 2 Jan") + " (all day)"
	}
	out := s.Format("Mon 2 Jan 15:04")
	if !e.End.IsZero() {
		end := e.End.In(t.cfg.Location)
		if end.YearDay() == s.YearDay() && end.Year() == s.Year() {
			out += "–" + end.Format("15:04")
		} else {
			out += " – " + end.Format("Mon 2 Jan 15:04")
		}
	}
	return out + " " + s.Format("MST")
}

func title(e Event) string {
	if strings.TrimSpace(e.Summary) == "" {
		return "(untitled event)"
	}
	return e.Summary
}

func (t *Tracker) base(account, typ string, e Event, at time.Time) sources.Event {
	if at.IsZero() {
		at = e.Updated
	}
	data := map[string]any{
		"event_id":    e.ID,
		"calendar_id": orPrimary(e.CalendarID),
		"start":       stamp(e.Start),
		"end":         stamp(e.End),
		"all_day":     e.AllDay,
		"status":      e.Status,
	}
	if e.Organizer != "" {
		data["organizer"] = e.Organizer
	}
	if e.OrganizerSelf {
		data["organizer_self"] = true
	}
	if e.MyResponse != "" {
		data["my_response"] = e.MyResponse
	}
	if e.Location != "" {
		data["location"] = e.Location
	}
	if e.Meet != "" {
		data["meet"] = e.Meet
	}
	if e.RecurringID != "" {
		data["recurring_id"] = e.RecurringID
	}
	if len(e.Attendees) > 0 {
		var emails []string
		for i, a := range e.Attendees {
			if i == 25 {
				break
			}
			emails = append(emails, a.Email)
		}
		data["attendees"] = emails
	}
	return sources.Event{Source: "gcal", Account: account, Type: typ, URL: e.Link, At: at, Data: data}
}

func orPrimary(id string) string {
	if id == "" {
		return "primary"
	}
	return id
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// detail is the summary a decision is made from.
func (t *Tracker) detail(account string, e Event, lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		if l != "" {
			b.WriteString(l + "\n")
		}
	}
	fmt.Fprintf(&b, "When: %s\n", t.when(e))
	if e.Location != "" {
		fmt.Fprintf(&b, "Where: %s\n", e.Location)
	}
	if e.Meet != "" {
		fmt.Fprintf(&b, "Video: %s\n", e.Meet)
	}
	switch {
	case e.OrganizerSelf:
		b.WriteString("Organizer: the operator (this account)\n")
	case e.Organizer != "":
		fmt.Fprintf(&b, "Organizer: %s\n", e.Organizer)
	}
	if e.MyResponse != "" {
		fmt.Fprintf(&b, "The operator's response: %s\n", e.MyResponse)
	}
	if n := len(e.Attendees); n > 0 {
		var names []string
		for i, a := range e.Attendees {
			if i == 8 {
				names = append(names, fmt.Sprintf("and %d more", n-8))
				break
			}
			s := who(a)
			if a.Response != "" {
				s += " (" + a.Response + ")"
			}
			names = append(names, s)
		}
		fmt.Fprintf(&b, "Guests: %s\n", strings.Join(names, ", "))
	}
	if d := strings.TrimSpace(e.Description); d != "" {
		if r := []rune(d); len(r) > 500 {
			d = string(r[:500]) + "…"
		}
		fmt.Fprintf(&b, "Description: %s\n", d)
	}
	fmt.Fprintf(&b, "Calendar account: %s (act on it as this account: event_id %s, calendar_id %s)", account, e.ID, orPrimary(e.CalendarID))
	return b.String()
}

func (t *Tracker) invitation(account string, e Event, what []string) sources.Event {
	ev := t.base(account, "invitation", e, time.Time{})
	from := e.Organizer
	if from == "" {
		from = "someone"
	}
	ev.Key = key(account, e.ID, "invite", stamp(e.Start))
	ev.Title = fmt.Sprintf("Invitation: %s — %s (from %s)", title(e), t.when(e), from)
	lead := "Awaiting the operator's response."
	if len(what) > 0 {
		lead = "Asked again: " + strings.Join(what, "; ") + ". Awaiting the operator's response."
	}
	ev.Summary = t.detail(account, e, lead)
	return ev
}

func (t *Tracker) upcoming(account string, e Event) sources.Event {
	ev := t.base(account, "event.upcoming", e, time.Time{})
	ev.Key = key(account, e.ID, "upcoming", stamp(e.Start))
	ev.Title = fmt.Sprintf("Coming up: %s — %s", title(e), t.when(e))
	ev.Summary = t.detail(account, e)
	return ev
}

func (t *Tracker) created(account string, e Event) sources.Event {
	ev := t.base(account, "event.created", e, time.Time{})
	ev.Key = key(account, e.ID, "created")
	ev.Title = fmt.Sprintf("New event: %s — %s", title(e), t.when(e))
	ev.Summary = t.detail(account, e)
	return ev
}

func (t *Tracker) changed(account string, prev, e Event, what []string) sources.Event {
	ev := t.base(account, "event.changed", e, time.Time{})
	ev.Key = key(account, e.ID, "changed", stamp(e.Updated), stamp(e.Start), strings.Join(what, ";"))
	name := title(e)
	if e.Summary == "" && prev.Summary != "" {
		name = prev.Summary
	}
	ev.Title = fmt.Sprintf("Changed: %s — %s", name, strings.Join(what, "; "))
	if !prev.Start.Equal(e.Start) && !prev.Start.IsZero() {
		ev.Title = fmt.Sprintf("Moved: %s — now %s", name, t.when(e))
	}
	ev.Summary = t.detail(account, e, "What changed: "+strings.Join(what, "; ")+".")
	return ev
}

func (t *Tracker) cancelled(account string, was Event, at time.Time) sources.Event {
	ev := t.base(account, "event.cancelled", was, at)
	ev.Data["status"] = "cancelled"
	ev.Key = key(account, was.ID, "cancelled")
	ev.Title = fmt.Sprintf("Cancelled: %s — was %s", title(was), t.when(was))
	ev.Summary = t.detail(account, was, "This event was cancelled.")
	return ev
}

func (t *Tracker) startingSoon(account string, e Event, now time.Time) sources.Event {
	ev := t.base(account, "event.starting_soon", e, now)
	ev.Urgent = true
	ev.Key = key(account, e.ID, "soon", stamp(e.Start))
	mins := int(e.Start.Sub(now).Round(time.Minute) / time.Minute)
	ev.Title = fmt.Sprintf("Starting in %d min: %s", mins, title(e))
	if e.Meet != "" {
		ev.Title += " (" + e.Meet + ")"
	}
	ev.Summary = t.detail(account, e)
	return ev
}

func key(account string, parts ...string) string {
	return "gcal|" + strings.ToLower(account) + "|" + strings.Join(parts, "|")
}
