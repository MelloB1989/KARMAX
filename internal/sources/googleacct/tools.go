package googleacct

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/sources/gcal"
	"github.com/MelloB1989/karmax/internal/tools"
)

// Tools are the actions KARMAX takes as the operator on Google: answer, move
// and create calendar events, and write in Chat — each as a named account.
//
// bound is the account the tools act as when a call names none: the account
// an event came from, when the tools are lent for that event's turn. Unbound
// with several accounts signed in, a call that names none is refused with the
// choices, because acting as the wrong account is the one mistake here that
// cannot be taken back quietly.
//
// sent hears what each action said or did to other people as the operator
// (tools.SentAs), once it has succeeded, so the operator is told after.
func Tools(c *Client, loc *time.Location, bound string, sent tools.SentRecorder) []tools.Tool {
	if loc == nil {
		loc = time.UTC
	}
	b := base{c: c, loc: loc, bound: strings.ToLower(strings.TrimSpace(bound)), sent: sent}
	return []tools.Tool{&accountsTool{b}, &sendTool{b}, &respondTool{b}, &rescheduleTool{b}, &createTool{b}}
}

// ToolNames lists the names Tools returns, for callers that grant them.
func ToolNames() []string {
	return []string{"google.accounts", "gchat.send", "gcal.respond", "gcal.reschedule", "gcal.create"}
}

type base struct {
	c     *Client
	loc   *time.Location
	bound string
	sent  tools.SentRecorder
}

// when is a time as the operator reads it, in their zone.
func (b base) when(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(b.loc).Format("Mon 2 Jan 15:04")
}

// invitation names a calendar event for telling the operator: its title, and
// whose it is when that is not the operator's.
func invitation(ev gcal.Event, id string) string {
	what := "an event"
	if s := strings.TrimSpace(ev.Summary); s != "" {
		what = "“" + s + "”"
	} else if id != "" {
		what = "event " + id
	}
	if ev.Organizer != "" && !ev.OrganizerSelf {
		what += " (" + ev.Organizer + ")"
	}
	return what
}

// guests are an event's attendees other than the account itself.
func guests(ev gcal.Event) []string {
	var out []string
	for _, a := range ev.Attendees {
		if !a.Self && a.Email != "" {
			out = append(out, a.Email)
		}
	}
	return out
}

const accountParam = `"account": {"type": "string", "description": "The Google address to act as — the account the event or message came from. Optional only when one account is signed in, or the tool was lent for an event."}`

// account resolves which account a call acts as.
func (b base) account(ctx context.Context, in map[string]any) (string, error) {
	if a, _ := in["account"].(string); strings.TrimSpace(a) != "" {
		return strings.ToLower(strings.TrimSpace(a)), nil
	}
	if b.bound != "" {
		return b.bound, nil
	}
	emails := b.c.h.Accounts(ctx).Emails()
	switch len(emails) {
	case 0:
		return "", fmt.Errorf("no Google account is signed in on this host; add one with `gog auth add you@example.com --services calendar,chat`")
	case 1:
		return emails[0], nil
	}
	sort.Strings(emails)
	return "", fmt.Errorf("say which account to act as: %s", strings.Join(emails, ", "))
}

// readTime reads an ISO-8601 time; one without a zone is the operator's.
func (b base) readTime(in map[string]any, key string) (time.Time, error) {
	s, _ := in[key].(string)
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, b.loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%s %q is not an ISO-8601 time like 2026-11-05T15:00:00+05:30", key, s)
}

func str(in map[string]any, key string) string {
	s, _ := in[key].(string)
	return strings.TrimSpace(s)
}

func boolOr(in map[string]any, key string, def bool) bool {
	if v, ok := in[key].(bool); ok {
		return v
	}
	return def
}

func strList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		var out []string
		for _, s := range strings.Split(x, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// --- google.accounts

type accountsTool struct{ base }

func (t *accountsTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "google.accounts",
		Description: "List the operator's Google accounts this host can act as for Calendar and Chat, " +
			"how each is reached (gog or gws), and — for any that is signed out — the exact command that fixes it.",
		Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (t *accountsTool) Execute(ctx context.Context, _ map[string]any) (tools.ToolResult, error) {
	t.c.h.Forget()
	d := t.c.h.Accounts(ctx)
	var out []map[string]any
	for _, a := range d.Accounts {
		var logins []map[string]any
		for _, l := range a.Logins {
			m := map[string]any{"via": l.String()}
			if len(l.Services) > 0 {
				m["services"] = l.Services
			}
			if l.Problem != "" {
				m["problem"] = l.Problem
				m["fix"] = l.Fix()
			}
			logins = append(logins, m)
		}
		entry := map[string]any{"account": a.Email, "logins": logins}
		if h := t.c.Health(a.Email); h.Err != nil {
			entry["last_error"] = h.Err.Error()
		}
		out = append(out, entry)
	}
	res := map[string]any{"accounts": out}
	if len(d.Errors) > 0 {
		res["errors"] = d.Errors
	}
	if len(out) == 0 {
		res["hint"] = "No Google account is signed in. Add one with `gog auth add you@example.com --services calendar,chat` (or `gws auth login`)."
	}
	return tools.SuccessResult(res), nil
}

// --- gchat.send

type sendTool struct{ base }

func (t *sendTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "gchat.send",
		Description: "Send a Google Chat message as the operator, from a named Google account. To reply to a message, " +
			"pass its space and thread (both are in the gchat event's data). To write to a person directly, pass their " +
			"address as `to` instead of a space. Write in the operator's own voice.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				` + accountParam + `,
				"space": {"type": "string", "description": "The space, e.g. spaces/AAAA."},
				"thread": {"type": "string", "description": "The thread to reply in, e.g. spaces/AAAA/threads/BBBB."},
				"to": {"type": "string", "description": "A person's address, for a direct message instead of a space."},
				"text": {"type": "string"}
			},
			"required": ["text"]
		}`),
	}
}

func (t *sendTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	account, err := t.account(ctx, in)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	text, space, to := str(in, "text"), str(in, "space"), str(in, "to")
	var sent Sent
	switch {
	case space != "":
		sent, err = t.c.Send(ctx, account, space, str(in, "thread"), text)
	case to != "":
		sent, err = t.c.SendDirect(ctx, account, to, text)
	default:
		err = fmt.Errorf("say where: a space (and thread) to reply in, or `to` for a person's address")
	}
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	where := to
	if where == "" {
		where = sent.Space
	}
	if where == "" {
		where = space
	}
	t.sent.Record(tools.SentAs{Service: "gchat", Account: account, To: where, Text: text})
	return tools.SuccessResult(map[string]any{"sent_as": account, "message": sent.Name, "space": sent.Space, "thread": sent.Thread}), nil
}

// --- gcal.respond

type respondTool struct{ base }

func (t *respondTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "gcal.respond",
		Description: "Answer a Google Calendar invitation as the operator: accepted, declined or tentative, " +
			"optionally with a note to the organizer. The event_id and calendar_id are in the gcal event's data.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				` + accountParam + `,
				"event_id": {"type": "string"},
				"calendar_id": {"type": "string", "description": "Defaults to primary."},
				"response": {"type": "string", "enum": ["accepted", "declined", "tentative"]},
				"comment": {"type": "string", "description": "A note the organizer sees with the answer."}
			},
			"required": ["event_id", "response"]
		}`),
	}
}

func (t *respondTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	account, err := t.account(ctx, in)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	ev, err := t.c.Respond(ctx, account, str(in, "calendar_id"), str(in, "event_id"), str(in, "response"), str(in, "comment"))
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	t.sent.Record(tools.SentAs{Service: "gcal", Account: account, To: invitation(ev, str(in, "event_id")),
		Action: str(in, "response"), Text: str(in, "comment"), URL: ev.Link})
	return tools.SuccessResult(map[string]any{"answered_as": account, "event_id": ev.ID, "response": str(in, "response"), "summary": ev.Summary}), nil
}

// --- gcal.reschedule

type rescheduleTool struct{ base }

func (t *rescheduleTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "gcal.reschedule",
		Description: "Move a Google Calendar event the operator can edit (one they organise) to a new time. " +
			"Without an end it keeps its length. Guests are sent Google's update unless notify is false. " +
			"Times are ISO-8601; one without an offset is in the operator's timezone.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				` + accountParam + `,
				"event_id": {"type": "string"},
				"calendar_id": {"type": "string", "description": "Defaults to primary."},
				"start": {"type": "string"},
				"end": {"type": "string"},
				"notify": {"type": "boolean", "description": "Email the guests the change (default true)."}
			},
			"required": ["event_id", "start"]
		}`),
	}
}

func (t *rescheduleTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	account, err := t.account(ctx, in)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	start, err := t.readTime(in, "start")
	if err == nil && start.IsZero() {
		err = fmt.Errorf("a new start is needed")
	}
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	end, err := t.readTime(in, "end")
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	ev, err := t.c.Reschedule(ctx, account, str(in, "calendar_id"), str(in, "event_id"), start, end, boolOr(in, "notify", true))
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	// Moving an event other people are in changes their day too, and is told
	// after; moving a private one is not something sent.
	if len(guests(ev)) > 0 {
		t.sent.Record(tools.SentAs{Service: "gcal", Account: account, To: invitation(ev, str(in, "event_id")),
			Action: "moved", Text: "to " + t.when(start), URL: ev.Link})
	}
	return tools.SuccessResult(map[string]any{"moved_as": account, "event_id": ev.ID, "start": stamp(ev.Start), "end": stamp(ev.End), "link": ev.Link}), nil
}

// --- gcal.create

type createTool struct{ base }

func (t *createTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "gcal.create",
		Description: "Create an event on one of the operator's Google Calendars, optionally inviting guests and adding a Meet link. " +
			"Guests are sent Google's invitation unless notify is false. Times are ISO-8601; one without an offset is in " +
			"the operator's timezone; without an end the event lasts 30 minutes.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				` + accountParam + `,
				"title": {"type": "string"},
				"start": {"type": "string"},
				"end": {"type": "string"},
				"attendees": {"type": "array", "items": {"type": "string"}, "description": "Guests' addresses."},
				"description": {"type": "string"},
				"location": {"type": "string"},
				"meet": {"type": "boolean", "description": "Add a Google Meet link."},
				"notify": {"type": "boolean", "description": "Email the guests the invitation (default true)."},
				"calendar_id": {"type": "string", "description": "Defaults to primary."}
			},
			"required": ["title", "start"]
		}`),
	}
}

func (t *createTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	account, err := t.account(ctx, in)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	start, err := t.readTime(in, "start")
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	end, err := t.readTime(in, "end")
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	attendees := strList(in["attendees"])
	ev, err := t.c.Create(ctx, account, NewEvent{
		CalendarID: str(in, "calendar_id"), Title: str(in, "title"), Start: start, End: end,
		Attendees: attendees, Description: str(in, "description"), Location: str(in, "location"),
		Meet: boolOr(in, "meet", false), Notify: boolOr(in, "notify", true),
	})
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	// An invitation is sent as the operator; an entry nobody is invited to is not.
	if len(attendees) > 0 {
		t.sent.Record(tools.SentAs{Service: "gcal", Account: account, To: strings.Join(attendees, ", "), Action: "invited",
			Text: fmt.Sprintf("“%s” on %s", str(in, "title"), t.when(start)), URL: ev.Link})
	}
	return tools.SuccessResult(map[string]any{"created_as": account, "event_id": ev.ID, "link": ev.Link, "meet": ev.Meet}), nil
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
