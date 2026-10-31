package googleacct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/sources/gcal"
)

// apiEvent is a Google Calendar event as the API (and so both CLIs) writes it.
type apiEvent struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	Summary     string  `json:"summary"`
	Description string  `json:"description"`
	Location    string  `json:"location"`
	HTMLLink    string  `json:"htmlLink"`
	HangoutLink string  `json:"hangoutLink"`
	Start       apiTime `json:"start"`
	End         apiTime `json:"end"`
	Created     string  `json:"created"`
	Updated     string  `json:"updated"`
	Organizer   struct {
		Email string `json:"email"`
		Self  bool   `json:"self"`
	} `json:"organizer"`
	Attendees []struct {
		Email          string `json:"email"`
		DisplayName    string `json:"displayName"`
		ResponseStatus string `json:"responseStatus"`
		Self           bool   `json:"self"`
		Organizer      bool   `json:"organizer"`
	} `json:"attendees"`
	RecurringEventID string `json:"recurringEventId"`
	EventType        string `json:"eventType"`
	ConferenceData   struct {
		EntryPoints []struct {
			EntryPointType string `json:"entryPointType"`
			URI            string `json:"uri"`
		} `json:"entryPoints"`
	} `json:"conferenceData"`
}

type apiTime struct {
	DateTime string `json:"dateTime"`
	Date     string `json:"date"`
}

func (t apiTime) parse(loc *time.Location) (time.Time, bool) {
	if t.DateTime != "" {
		v, err := time.Parse(time.RFC3339, t.DateTime)
		return v, err == nil
	}
	if t.Date != "" {
		v, err := time.ParseInLocation("2006-01-02", t.Date, loc)
		return v, err == nil
	}
	return time.Time{}, false
}

func parseStamp(s string) time.Time {
	v, _ := time.Parse(time.RFC3339Nano, s)
	return v
}

func (e apiEvent) toEvent(calendarID string, loc *time.Location) gcal.Event {
	out := gcal.Event{
		ID: e.ID, CalendarID: calendarID, Status: e.Status, Summary: e.Summary,
		Description: e.Description, Location: e.Location, Link: e.HTMLLink, Meet: e.HangoutLink,
		Created: parseStamp(e.Created), Updated: parseStamp(e.Updated),
		Organizer: e.Organizer.Email, OrganizerSelf: e.Organizer.Self,
		RecurringID: e.RecurringEventID, Type: e.EventType,
	}
	out.Start, _ = e.Start.parse(loc)
	out.End, _ = e.End.parse(loc)
	out.AllDay = e.Start.DateTime == "" && e.Start.Date != ""
	if out.Meet == "" {
		for _, p := range e.ConferenceData.EntryPoints {
			if p.EntryPointType == "video" {
				out.Meet = p.URI
				break
			}
		}
	}
	for _, a := range e.Attendees {
		out.Attendees = append(out.Attendees, gcal.Attendee{Email: a.Email, Name: a.DisplayName,
			Response: a.ResponseStatus, Self: a.Self, Organizer: a.Organizer})
		if a.Self && !a.Organizer {
			out.MyResponse = a.ResponseStatus
		}
	}
	return out
}

func calendarOr(id string) string {
	if strings.TrimSpace(id) == "" {
		return "primary"
	}
	return id
}

// Events lists one account's primary calendar (gcal.API).
//
// gws speaks sync tokens: the whole window once, then only what changed. gog
// cannot, so it lists the window every time, and asks separately for what
// changed since the last listing, because its window listing leaves cancelled
// events out.
func (c *Client) Events(ctx context.Context, account string, q gcal.Query) (gcal.Page, error) {
	var page gcal.Page
	err := c.with(ctx, account, "calendar", func(l Login) error {
		var err error
		if l.Path == Gws {
			page, err = c.gwsEvents(ctx, l, q)
		} else {
			page, err = c.gogEvents(ctx, l, q)
		}
		return err
	})
	return page, err
}

func (c *Client) gwsEvents(ctx context.Context, l Login, q gcal.Query) (gcal.Page, error) {
	params := map[string]any{"calendarId": "primary", "singleEvents": true, "maxResults": 250}
	page := gcal.Page{}
	if q.SyncToken != "" {
		params["syncToken"] = q.SyncToken
	} else {
		params["timeMin"] = q.From.UTC().Format(time.RFC3339)
		params["timeMax"] = q.To.UTC().Format(time.RFC3339)
		params["showDeleted"] = true
		page.Complete = true
	}
	err := c.gwsPages(ctx, l, "calendar events list", func(raw []byte) error {
		var p struct {
			Items         []apiEvent `json:"items"`
			NextSyncToken string     `json:"nextSyncToken"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		for _, e := range p.Items {
			page.Events = append(page.Events, e.toEvent("primary", c.loc))
		}
		if p.NextSyncToken != "" {
			page.SyncToken = p.NextSyncToken
		}
		return nil
	}, "calendar", "events", "list", "--params", jsonArg(params))
	var ce *cliError
	if q.SyncToken != "" && errors.As(err, &ce) && ce.gone {
		return gcal.Page{}, gcal.ErrSyncExpired
	}
	return page, err
}

func (c *Client) gogEvents(ctx context.Context, l Login, q gcal.Query) (gcal.Page, error) {
	var listed struct {
		Events []apiEvent `json:"events"`
	}
	if err := c.gog(ctx, l, "calendar events", &listed, "calendar", "events", "primary",
		"--from", q.From.UTC().Format(time.RFC3339), "--to", q.To.UTC().Format(time.RFC3339),
		"--max", "250", "--all-pages"); err != nil {
		return gcal.Page{}, err
	}
	page := gcal.Page{Complete: true}
	for _, e := range listed.Events {
		page.Events = append(page.Events, e.toEvent("primary", c.loc))
	}
	if !q.Since.IsZero() {
		var changed struct {
			Events []apiEvent `json:"events"`
		}
		if err := c.gog(ctx, l, "calendar changed", &changed, "calendar", "changed", "primary",
			"--since", q.Since.Add(-5*time.Minute).UTC().Format(time.RFC3339), "--max", "2500"); err != nil {
			return gcal.Page{}, err
		}
		for _, e := range changed.Events {
			if e.Status == "cancelled" {
				page.Events = append(page.Events, e.toEvent("primary", c.loc))
			}
		}
	}
	return page, nil
}

// Event reads one event.
func (c *Client) Event(ctx context.Context, account, calendarID, eventID string) (gcal.Event, error) {
	calendarID = calendarOr(calendarID)
	var out gcal.Event
	err := c.with(ctx, account, "calendar", func(l Login) error {
		var e apiEvent
		if l.Path == Gws {
			if err := c.gws(ctx, l, "calendar events get", &e, "calendar", "events", "get",
				"--params", jsonArg(map[string]any{"calendarId": calendarID, "eventId": eventID})); err != nil {
				return err
			}
		} else {
			var wrapped struct {
				Event apiEvent `json:"event"`
			}
			if err := c.gog(ctx, l, "calendar event", &wrapped, "calendar", "event", calendarID, eventID); err != nil {
				return err
			}
			e = wrapped.Event
		}
		out = e.toEvent(calendarID, c.loc)
		return nil
	})
	return out, err
}

// Respond answers an invitation as the account.
func (c *Client) Respond(ctx context.Context, account, calendarID, eventID, response, comment string) (gcal.Event, error) {
	switch response {
	case "accepted", "declined", "tentative":
	default:
		return gcal.Event{}, fmt.Errorf("response must be accepted, declined or tentative, not %q", response)
	}
	calendarID = calendarOr(calendarID)
	var out gcal.Event
	err := c.with(ctx, account, "calendar", func(l Login) error {
		if l.Path == Gog {
			args := []string{"calendar", "respond", calendarID, eventID, "--status", response}
			if comment != "" {
				args = append(args, "--comment", comment)
			}
			var wrapped struct {
				Event apiEvent `json:"event"`
			}
			if err := c.gog(ctx, l, "calendar respond", &wrapped, args...); err != nil {
				return err
			}
			out = wrapped.Event.toEvent(calendarID, c.loc)
			return nil
		}
		// The raw event is patched with its own attendee list, changed only
		// in the account's own entry, so no other guest's answer is touched.
		var raw map[string]any
		if err := c.gws(ctx, l, "calendar events get", &raw, "calendar", "events", "get",
			"--params", jsonArg(map[string]any{"calendarId": calendarID, "eventId": eventID})); err != nil {
			return err
		}
		attendees, _ := raw["attendees"].([]any)
		found := false
		for _, a := range attendees {
			m, ok := a.(map[string]any)
			if !ok {
				continue
			}
			if self, _ := m["self"].(bool); self {
				m["responseStatus"] = response
				if comment != "" {
					m["comment"] = comment
				}
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s is not a guest of event %s, so there is nothing to answer", account, eventID)
		}
		var e apiEvent
		if err := c.gws(ctx, l, "calendar events patch", &e, "calendar", "events", "patch",
			"--params", jsonArg(map[string]any{"calendarId": calendarID, "eventId": eventID}),
			"--json", jsonArg(map[string]any{"attendees": attendees})); err != nil {
			return err
		}
		out = e.toEvent(calendarID, c.loc)
		return nil
	})
	return out, err
}

// Reschedule moves an event the account can edit. A zero end keeps the
// event's length. notify sends Google's update to the guests.
func (c *Client) Reschedule(ctx context.Context, account, calendarID, eventID string, start, end time.Time, notify bool) (gcal.Event, error) {
	calendarID = calendarOr(calendarID)
	if end.IsZero() {
		cur, err := c.Event(ctx, account, calendarID, eventID)
		if err != nil {
			return gcal.Event{}, err
		}
		end = start.Add(cur.End.Sub(cur.Start))
	}
	if !end.After(start) {
		return gcal.Event{}, fmt.Errorf("the new end (%s) is not after the new start (%s)", end.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	send := sendUpdates(notify)
	var out gcal.Event
	err := c.with(ctx, account, "calendar", func(l Login) error {
		if l.Path == Gog {
			var wrapped struct {
				Event apiEvent `json:"event"`
			}
			if err := c.gog(ctx, l, "calendar update", &wrapped, "calendar", "update", calendarID, eventID,
				"--from", start.Format(time.RFC3339), "--to", end.Format(time.RFC3339), "--send-updates", send); err != nil {
				return err
			}
			out = wrapped.Event.toEvent(calendarID, c.loc)
			return nil
		}
		var e apiEvent
		if err := c.gws(ctx, l, "calendar events patch", &e, "calendar", "events", "patch",
			"--params", jsonArg(map[string]any{"calendarId": calendarID, "eventId": eventID, "sendUpdates": send}),
			"--json", jsonArg(map[string]any{
				"start": map[string]any{"dateTime": start.Format(time.RFC3339)},
				"end":   map[string]any{"dateTime": end.Format(time.RFC3339)},
			})); err != nil {
			return err
		}
		out = e.toEvent(calendarID, c.loc)
		return nil
	})
	return out, err
}

// NewEvent is an event to create.
type NewEvent struct {
	CalendarID  string
	Title       string
	Start, End  time.Time
	Attendees   []string
	Description string
	Location    string
	Meet        bool // add a Google Meet link
	Notify      bool // send the guests Google's invitation
}

// Create puts an event on the account's calendar.
func (c *Client) Create(ctx context.Context, account string, n NewEvent) (gcal.Event, error) {
	if strings.TrimSpace(n.Title) == "" || n.Start.IsZero() {
		return gcal.Event{}, fmt.Errorf("an event needs a title and a start")
	}
	if n.End.IsZero() {
		n.End = n.Start.Add(30 * time.Minute)
	}
	if !n.End.After(n.Start) {
		return gcal.Event{}, fmt.Errorf("the end is not after the start")
	}
	cal := calendarOr(n.CalendarID)
	send := sendUpdates(n.Notify)
	var out gcal.Event
	err := c.with(ctx, account, "calendar", func(l Login) error {
		if l.Path == Gog {
			args := []string{"calendar", "create", cal, "--summary", n.Title,
				"--from", n.Start.Format(time.RFC3339), "--to", n.End.Format(time.RFC3339), "--send-updates", send}
			if n.Description != "" {
				args = append(args, "--description", n.Description)
			}
			if n.Location != "" {
				args = append(args, "--location", n.Location)
			}
			if len(n.Attendees) > 0 {
				args = append(args, "--attendees", strings.Join(n.Attendees, ","))
			}
			if n.Meet {
				args = append(args, "--with-meet")
			}
			var wrapped struct {
				Event apiEvent `json:"event"`
			}
			if err := c.gog(ctx, l, "calendar create", &wrapped, args...); err != nil {
				return err
			}
			out = wrapped.Event.toEvent(cal, c.loc)
			return nil
		}
		body := map[string]any{
			"summary": n.Title,
			"start":   map[string]any{"dateTime": n.Start.Format(time.RFC3339)},
			"end":     map[string]any{"dateTime": n.End.Format(time.RFC3339)},
		}
		if n.Description != "" {
			body["description"] = n.Description
		}
		if n.Location != "" {
			body["location"] = n.Location
		}
		if len(n.Attendees) > 0 {
			var as []map[string]any
			for _, a := range n.Attendees {
				as = append(as, map[string]any{"email": a})
			}
			body["attendees"] = as
		}
		params := map[string]any{"calendarId": cal, "sendUpdates": send}
		if n.Meet {
			body["conferenceData"] = map[string]any{"createRequest": map[string]any{
				"requestId":             fmt.Sprintf("karmax-%d", time.Now().UnixNano()),
				"conferenceSolutionKey": map[string]any{"type": "hangoutsMeet"},
			}}
			params["conferenceDataVersion"] = 1
		}
		var e apiEvent
		if err := c.gws(ctx, l, "calendar events insert", &e, "calendar", "events", "insert",
			"--params", jsonArg(params), "--json", jsonArg(body)); err != nil {
			return err
		}
		out = e.toEvent(cal, c.loc)
		return nil
	})
	return out, err
}

func sendUpdates(notify bool) string {
	if notify {
		return "all"
	}
	return "none"
}
