package googleacct

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources/gcal"
)

const acctAddr = "priya@example.com"

// gwsOnly is a host whose one login for the account is gws.
func gwsOnly() (*fakeCLI, *Client) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatus(acctAddr, true)})
	return f, NewClient(hostWith(f, "/cfg/gws"), time.UTC)
}

// gogOnly is a host whose one login for the account is gog.
func gogOnly() (*fakeCLI, *Client) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[{"email":"priya@example.com","services":["calendar","chat"],"auth":"oauth"}]}`})
	h := hostWith(f)
	h.Gws = ""
	return f, NewClient(h, time.UTC)
}

func params(t *testing.T, c call) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(argValue(c.args, "--params")), &m); err != nil {
		t.Fatalf("params of %v: %v", c, err)
	}
	return m
}

func body(t *testing.T, c call) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(argValue(c.args, "--json")), &m); err != nil {
		t.Fatalf("body of %v: %v", c, err)
	}
	return m
}

func TestGwsListsTheWindowThenUsesItsSyncToken(t *testing.T) {
	f, c := gwsOnly()
	f.on("gws", "", "calendar events list syncToken", reply{errOut: `{"error":{"code":410,"message":"Sync token is no longer valid, a full sync is required."}}`, code: 1})
	f.on("gws", "", "calendar events list timeMin", reply{out: strings.ReplaceAll(calPage, "\n", "") + "\n"})
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	page, err := c.Events(context.Background(), acctAddr, gcal.Query{From: from, To: from.Add(15 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	got := f.find(t, "gws", "calendar", "events", "list")
	p := params(t, got)
	if p["calendarId"] != "primary" || p["singleEvents"] != true || p["showDeleted"] != true || p["timeMin"] != "2026-11-01T00:00:00Z" {
		t.Errorf("window params %v", p)
	}
	if got.envValue("GOOGLE_WORKSPACE_CLI_CONFIG_DIR") != "/cfg/gws" || !strings.Contains(strings.Join(got.args, " "), "--page-all") {
		t.Errorf("gws must run in the login's config dir, paginated: %v", got)
	}
	if !page.Complete || page.SyncToken != "sync-1" || len(page.Events) != 1 {
		t.Fatalf("page %+v", page)
	}
	e := page.Events[0]
	if e.MyResponse != "needsAction" || e.Organizer != "rahul@example.com" || e.Meet == "" || e.Updated.IsZero() || e.Start.UTC().Hour() != 4 {
		t.Errorf("event %+v", e)
	}

	_, err = c.Events(context.Background(), acctAddr, gcal.Query{SyncToken: "sync-1"})
	if !errors.Is(err, gcal.ErrSyncExpired) {
		t.Fatalf("a 410 on a token must read as an expired token, got %v", err)
	}
	if h := c.Health(acctAddr); h.Err != nil {
		t.Errorf("an expired token is not the account being unreachable: %v", h.Err)
	}
}

func TestGogListsTheWindowAndAsksSeparatelyForCancellations(t *testing.T) {
	f, c := gogOnly()
	f.on("gog", "", "calendar events primary", reply{out: `{"events":[{"id":"a","status":"confirmed","summary":"Offsite","start":{"date":"2026-11-04"},"end":{"date":"2026-11-05"}}],"nextPageToken":""}`})
	f.on("gog", "", "calendar changed primary", reply{out: `{"events":[{"id":"b","status":"cancelled","updated":"2026-11-01T10:00:00Z"},{"id":"a","status":"confirmed"}],"since":"x"}`})
	since := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	page, err := c.Events(context.Background(), acctAddr, gcal.Query{From: since, To: since.Add(time.Hour), Since: since})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Complete || len(page.Events) != 2 || page.Events[1].ID != "b" || page.Events[1].Status != "cancelled" {
		t.Fatalf("page %+v", page)
	}
	if a := page.Events[0]; !a.AllDay || a.Start.Day() != 4 {
		t.Errorf("all-day event %+v", a)
	}
	ch := f.find(t, "gog", "calendar", "changed")
	if argValue(ch.args, "--since") == "" || argValue(ch.args, "--account") != acctAddr {
		t.Errorf("changed call %v", ch)
	}
}

const chatMsgs = `{"messages":[
 {"name":"spaces/S1/messages/m1","text":"hey @Priya can you check","createTime":"2026-11-01T10:00:00.5Z","sender":{"name":"users/300","type":"HUMAN"},"thread":{"name":"spaces/S1/threads/t1"},
  "annotations":[{"type":"USER_MENTION","userMention":{"user":{"name":"users/100"},"type":"MENTION"}}]},
 {"name":"spaces/S1/messages/m2","createTime":"2026-11-01T10:01:00Z","sender":{"name":"users/300","type":"HUMAN"},"deletionMetadata":{"deletionType":"CREATOR"}}
]}`

func TestGwsChat(t *testing.T) {
	f, c := gwsOnly()
	// The more specific rule first: "chat spaces list" is also in "chat spaces messages list".
	f.on("gws", "", "chat spaces messages list", reply{out: chatMsgs})
	f.on("gws", "", "chat spaces list", reply{out: `{"spaces":[{"name":"spaces/S1","displayName":"Launch crew","spaceType":"SPACE","lastActiveTime":"2026-11-01T10:01:00Z"},{"name":"spaces/D1","spaceType":"DIRECT_MESSAGE"}]}` + "\n"})
	f.on("gws", "", "people people get people/me", reply{out: `{"resourceName":"people/100","names":[{"displayName":"Priya Sharma"}]}`})

	spaces, err := c.Spaces(context.Background(), acctAddr)
	if err != nil || len(spaces) != 2 || spaces[0].LastActive.IsZero() || spaces[1].Type != "DIRECT_MESSAGE" {
		t.Fatalf("spaces %+v %v", spaces, err)
	}
	after := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	msgs, err := c.Messages(context.Background(), acctAddr, "spaces/S1", after, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("a deleted message was kept: %+v", msgs)
	}
	if m := msgs[0]; m.Sender != "users/300" || m.SenderType != "HUMAN" || len(m.Mentions) != 1 || m.Mentions[0] != "users/100" || m.Thread != "spaces/S1/threads/t1" {
		t.Errorf("message %+v", m)
	}
	p := params(t, f.find(t, "gws", "chat", "messages", "list"))
	if p["parent"] != "spaces/S1" || p["filter"] != `createTime > "2026-11-01T09:00:00Z"` {
		t.Errorf("messages params %v", p)
	}
	me, err := c.Me(context.Background(), acctAddr)
	if err != nil || me.User != "users/100" || me.Name != "Priya Sharma" {
		t.Fatalf("me %+v %v", me, err)
	}
}

// The People API can be off in the OAuth app's project; Chat itself can still
// say who the account is.
func TestGwsMeFallsBackToTheAccountsOwnMembership(t *testing.T) {
	f, c := gwsOnly()
	f.on("gws", "", "people people get", reply{errOut: `{"error":{"code":403,"message":"People API has not been used in project"}}`, code: 1})
	f.on("gws", "", "chat spaces list", reply{out: `{"spaces":[{"name":"spaces/S1"}]}`})
	f.on("gws", "", "chat spaces members get spaces/S1/members/priya@example.com", reply{out: `{"name":"spaces/S1/members/100","member":{"name":"users/100","type":"HUMAN"}}`})
	me, err := c.Me(context.Background(), acctAddr)
	if err != nil || me.User != "users/100" {
		t.Fatalf("me %+v %v", me, err)
	}
}

func TestGogChatIsReadNewestFirstAndTurnedAround(t *testing.T) {
	f, c := gogOnly()
	f.on("gog", "", "chat messages list spaces/S1", reply{out: `{"messages":[
	 {"resource":"spaces/S1/messages/3","sender":"Rahul Verma","text":"third","createTime":"2026-11-01T10:03:00Z","thread":"spaces/S1/threads/c"},
	 {"resource":"spaces/S1/messages/2","sender":"users/300","text":"second","createTime":"2026-11-01T10:02:00Z"},
	 {"resource":"spaces/S1/messages/1","sender":"users/300","text":"first","createTime":"2026-11-01T10:01:00Z"}]}`})
	after := time.Date(2026, 11, 1, 10, 1, 30, 0, time.UTC)
	msgs, err := c.Messages(context.Background(), acctAddr, "spaces/S1", after, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Text != "second" || msgs[1].Text != "third" {
		t.Fatalf("messages %+v", msgs)
	}
	if msgs[0].Sender != "users/300" || msgs[1].Sender != "" || msgs[1].SenderName != "Rahul Verma" || msgs[1].Mentions != nil {
		t.Errorf("senders %+v", msgs)
	}
	got := f.find(t, "gog", "chat", "messages", "list")
	if argValue(got.args, "--order") != "createTime desc" || argValue(got.args, "--account") != acctAddr {
		t.Errorf("gog call %v", got)
	}
}

func TestRespondingChangesOnlyTheAccountsOwnAnswer(t *testing.T) {
	f, c := gwsOnly()
	f.on("gws", "", "calendar events get", reply{out: `{"id":"e1","attendees":[{"email":"rahul@example.com","organizer":true,"responseStatus":"accepted"},{"email":"priya@example.com","self":true,"responseStatus":"needsAction"}]}`})
	f.on("gws", "", "calendar events patch", reply{out: `{"id":"e1","status":"confirmed"}`})
	if _, err := c.Respond(context.Background(), acctAddr, "", "e1", "declined", "clash with travel"); err != nil {
		t.Fatal(err)
	}
	patch := f.find(t, "gws", "calendar", "events", "patch")
	b := body(t, patch)
	as := b["attendees"].([]any)
	if len(b) != 1 || len(as) != 2 {
		t.Fatalf("the patch should carry the attendee list only: %v", b)
	}
	if as[0].(map[string]any)["responseStatus"] != "accepted" || as[1].(map[string]any)["responseStatus"] != "declined" || as[1].(map[string]any)["comment"] != "clash with travel" {
		t.Errorf("attendees %v", as)
	}
	if _, err := c.Respond(context.Background(), acctAddr, "", "e1", "maybe", ""); err == nil {
		t.Error("an invalid response was accepted")
	}

	g, gc := gogOnly()
	g.on("gog", "", "calendar respond primary e2", reply{out: `{"event":{"id":"e2"}}`})
	if _, err := gc.Respond(context.Background(), acctAddr, "", "e2", "accepted", ""); err != nil {
		t.Fatal(err)
	}
	if r := g.find(t, "gog", "calendar", "respond"); argValue(r.args, "--status") != "accepted" {
		t.Errorf("gog respond %v", r)
	}
}

func TestReschedulingKeepsTheLengthAndSaysWhetherToNotify(t *testing.T) {
	f, c := gwsOnly()
	f.on("gws", "", "calendar events get", reply{out: `{"id":"e1","start":{"dateTime":"2026-11-03T10:00:00Z"},"end":{"dateTime":"2026-11-03T10:45:00Z"}}`})
	f.on("gws", "", "calendar events patch", reply{out: `{"id":"e1"}`})
	start := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	if _, err := c.Reschedule(context.Background(), acctAddr, "", "e1", start, time.Time{}, true); err != nil {
		t.Fatal(err)
	}
	patch := f.find(t, "gws", "calendar", "events", "patch")
	if params(t, patch)["sendUpdates"] != "all" {
		t.Errorf("params %v", params(t, patch))
	}
	if end := body(t, patch)["end"].(map[string]any)["dateTime"]; end != "2026-11-03T14:45:00Z" {
		t.Errorf("end %v: the event's 45 minutes should be kept", end)
	}

	g, gc := gogOnly()
	g.on("gog", "", "calendar update primary e9", reply{out: `{"event":{"id":"e9"}}`})
	if _, err := gc.Reschedule(context.Background(), acctAddr, "", "e9", start, start.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	u := g.find(t, "gog", "calendar", "update")
	if argValue(u.args, "--from") != "2026-11-03T14:00:00Z" || argValue(u.args, "--send-updates") != "none" {
		t.Errorf("gog update %v", u)
	}
}

func TestCreatingAnEvent(t *testing.T) {
	f, c := gwsOnly()
	f.on("gws", "", "calendar events insert", reply{out: `{"id":"new1","htmlLink":"https://calendar.example.test/new1"}`})
	start := time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC)
	ev, err := c.Create(context.Background(), acctAddr, NewEvent{Title: "Sync with Anita", Start: start,
		Attendees: []string{"anita@example.com"}, Meet: true, Notify: true})
	if err != nil || ev.ID != "new1" {
		t.Fatalf("create %+v %v", ev, err)
	}
	ins := f.find(t, "gws", "calendar", "events", "insert")
	p, b := params(t, ins), body(t, ins)
	if p["conferenceDataVersion"] != float64(1) || p["sendUpdates"] != "all" || b["conferenceData"] == nil {
		t.Errorf("a Meet link needs conferenceData and its version: %v %v", p, b)
	}
	if b["end"].(map[string]any)["dateTime"] != "2026-11-05T11:30:00Z" {
		t.Errorf("default length: %v", b["end"])
	}

	g, gc := gogOnly()
	g.on("gog", "", "calendar create primary", reply{out: `{"event":{"id":"new2"}}`})
	if _, err := gc.Create(context.Background(), acctAddr, NewEvent{Title: "Focus", Start: start, End: start.Add(time.Hour), Attendees: []string{"a@example.com", "b@example.com"}}); err != nil {
		t.Fatal(err)
	}
	cr := g.find(t, "gog", "calendar", "create")
	if argValue(cr.args, "--summary") != "Focus" || argValue(cr.args, "--attendees") != "a@example.com,b@example.com" {
		t.Errorf("gog create %v", cr)
	}
}

func TestSendingInAThreadAndDirectly(t *testing.T) {
	f, c := gwsOnly()
	f.on("gws", "", "chat spaces messages create", reply{out: `{"name":"spaces/S1/messages/r1","thread":{"name":"spaces/S1/threads/t1"}}`})
	f.on("gws", "", "chat spaces findDirectMessage", reply{errOut: `{"error":{"code":404,"message":"not found"}}`, code: 1})
	f.on("gws", "", "chat spaces setup", reply{out: `{"name":"spaces/D9","spaceType":"DIRECT_MESSAGE"}`})

	sent, err := c.Send(context.Background(), acctAddr, "spaces/S1", "spaces/S1/threads/t1", "on it")
	if err != nil || sent.Thread != "spaces/S1/threads/t1" {
		t.Fatalf("send %+v %v", sent, err)
	}
	cr := f.find(t, "gws", "chat", "messages", "create")
	if params(t, cr)["messageReplyOption"] != "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD" || body(t, cr)["thread"] == nil {
		t.Errorf("a reply must go into its thread: %v", cr)
	}

	sent, err = c.SendDirect(context.Background(), acctAddr, "anita@example.com", "hello")
	if err != nil || sent.Space != "spaces/D9" {
		t.Fatalf("direct %+v %v", sent, err)
	}
	if _, err := c.Send(context.Background(), acctAddr, "not-a-space", "", "x"); err == nil {
		t.Error("a malformed space was accepted")
	}

	g, gc := gogOnly()
	g.on("gog", "", "chat messages send spaces/S2", reply{out: `{"message":{"name":"spaces/S2/messages/x"}}`})
	if _, err := gc.Send(context.Background(), acctAddr, "spaces/S2", "spaces/S2/threads/q", "ok"); err != nil {
		t.Fatal(err)
	}
	if s := g.find(t, "gog", "chat", "messages", "send"); argValue(s.args, "--thread") != "spaces/S2/threads/q" || argValue(s.args, "--text") != "ok" {
		t.Errorf("gog send %v", s)
	}
}
