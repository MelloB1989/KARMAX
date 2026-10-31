package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/sources/googleacct"
	"github.com/MelloB1989/karmax/internal/tools"
)

func gmailMessageJSON(id string, at time.Time, labels string) string {
	return fmt.Sprintf(`{"id":%q,"threadId":%q,"labelIds":[%s],"snippet":"Could you sign by Friday?","internalDate":"%d",
"payload":{"mimeType":"text/plain","headers":[{"name":"From","value":"Dana Reyes <dana@example.org>"},
{"name":"To","value":"priya@example.com"},{"name":"Subject","value":"Lease renewal"}]}}`, id, id, labels, at.UnixMilli())
}

// The first look is backlog and pages nobody; mail that arrives afterwards
// is published to the primary agent on the account's route, urgent when a
// person writes to the operator about something Gmail marks important.
func TestTheGmailSourcePublishesNewInboxMail(t *testing.T) {
	r := newGoogleRig(t, noEnv)
	r.answers["gmail users getProfile"] = `{"emailAddress":"priya@example.com","historyId":"100"}`
	r.answers["gmail users messages list"] = `{"messages":[{"id":"old1","threadId":"old1"}]}`
	r.answers["gmail users messages get old1"] = gmailMessageJSON("old1", time.Now().Add(-time.Hour), `"INBOX","UNREAD","IMPORTANT"`)

	if err := r.g.tick(context.Background(), "gmail"); err != nil {
		t.Fatalf("first look: %v", err)
	}
	if len(r.published) != 1 || r.published[0].Payload["backlog"] != true || r.published[0].Payload["urgent"] != false {
		t.Fatalf("first look published %+v, want one backlog event that pages nobody", r.published)
	}

	r.answers["gmail users history list"] = `{"history":[{"id":"101","messagesAdded":[{"message":{"id":"new1","threadId":"new1","labelIds":["INBOX","UNREAD","IMPORTANT"]}}]}],"historyId":"120"}`
	r.answers["gmail users messages get new1"] = gmailMessageJSON("new1", time.Now(), `"INBOX","UNREAD","IMPORTANT"`)
	if err := r.g.tick(context.Background(), "gmail"); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(r.published) != 2 {
		t.Fatalf("published %d events", len(r.published))
	}
	e := r.published[1]
	if e.Kind != bus.EventWebhookFired || e.AgentID != "nexus" || e.Payload["route"] != "gmail:priya@example.com" ||
		e.Payload["urgent"] != true || e.Payload["key"] != "new1" {
		t.Fatalf("event %+v", e.Payload)
	}
	in := r.regs["google_account:priya@example.com"]
	if in == nil {
		t.Fatal("the account's health was not registered")
	}
	if err := in.Health(context.Background(), connectorCreds()); err != nil {
		t.Errorf("a working account reports unhealthy: %v", err)
	}
}

// A login that reads mail but cannot send or file it says so in health,
// with the exact command that grants the scope.
func TestAGmailLoginThatCannotSendSaysWhatToRunInHealth(t *testing.T) {
	r := newGoogleRig(t, noEnv)
	r.scopes = []string{"https://www.googleapis.com/auth/calendar", "https://www.googleapis.com/auth/gmail.readonly"}
	r.answers["gmail users getProfile"] = `{"historyId":"100"}`
	r.answers["gmail users messages list"] = `{"messages":[]}`
	if err := r.g.tick(context.Background(), "gmail"); err != nil {
		t.Fatalf("tick: %v", err)
	}
	err := r.regs["google_account:priya@example.com"].Health(context.Background(), connectorCreds())
	if err == nil {
		t.Fatal("a login that cannot send reports healthy")
	}
	for _, want := range []string{"send", "gws auth login --scopes", "https://www.googleapis.com/auth/calendar", "gmail.modify"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("health %q should say %q", err, want)
		}
	}
	for _, e := range r.published {
		if e.Payload["urgent"] == true {
			t.Errorf("a missing scope paged the operator: %+v", e.Payload)
		}
	}
}

// An account with no Gmail grant at all is not polled for mail, and its
// other sources carry on.
func TestAnAccountWithoutGmailIsNotReadForMail(t *testing.T) {
	r := newGoogleRig(t, noEnv)
	r.scopes = []string{"https://www.googleapis.com/auth/calendar"}
	if err := r.g.tick(context.Background(), "gmail"); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(r.published) != 0 {
		t.Fatalf("published %+v", r.published)
	}
}

func TestTheGmailSourcesSettings(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, _, on := gmailSourceConfig(noEnv); !on {
		t.Fatal("the Gmail source is on by default")
	}
	if _, _, on := gmailSourceConfig(env(map[string]string{"KARMAX_SOURCE_GMAIL": "off"})); on {
		t.Fatal("KARMAX_SOURCE_GMAIL=off ignored")
	}
	cfg, every, _ := gmailSourceConfig(env(map[string]string{
		"KARMAX_SOURCE_GMAIL_EVERY":           "5m",
		"KARMAX_SOURCE_GMAIL_SKIP_CATEGORIES": "promotions, social",
		"KARMAX_SOURCE_GMAIL_URGENT":          "none",
		"KARMAX_SOURCE_GMAIL_LOOKBACK":        "6h",
	}))
	if every != "5m" || strings.Join(cfg.SkipCategories, ",") != "promotions,social" || cfg.UrgentWhen == nil ||
		len(cfg.UrgentWhen) != 0 || cfg.FirstLook != 6*time.Hour {
		t.Errorf("config %+v every %q", cfg, every)
	}
	if cfg, every, _ := gmailSourceConfig(noEnv); cfg.SkipCategories != nil || cfg.UrgentWhen != nil || every != "2m" {
		t.Errorf("defaults %+v %q", cfg, every)
	}

	off := newGoogleRig(t, env(map[string]string{"KARMAX_SOURCE_GMAIL": "off"}))
	for _, l := range off.g.loops() {
		if l.Name == "gmail-source" {
			t.Fatal("the Gmail loop runs while switched off")
		}
	}
}

// The Google actions are lent to a Gmail event's turn, bound to its account,
// and what they send is recorded.
func TestGmailEventsAreLentTheGoogleActionsBoundToTheirAccount(t *testing.T) {
	r := newGoogleRig(t, noEnv)
	var got []tools.SentAs
	r.g.sent = func(s tools.SentAs) { got = append(got, s) }
	r.answers["gmail users messages send"] = `{"id":"s1","threadId":"s1"}`
	rt := &KarmaxRuntime{google: r.g}
	ev := bus.NewEvent(bus.EventWebhookFired, "nexus", map[string]any{"source": "gmail", "account": "priya@example.com"})
	lent := rt.googleToolsForEvent(ev)
	if len(lent) != len(googleacct.ToolNames()) {
		t.Fatalf("lent %d tools for a gmail event", len(lent))
	}
	var send tools.Tool
	for _, tl := range lent {
		if tl.Manifest().Name == "gmail.send" {
			send = tl
		}
	}
	res, err := send.Execute(context.Background(), map[string]any{"to": []any{"dana@example.org"}, "subject": "Lease", "text": "Signed and sent."})
	if err != nil || res.IsError {
		t.Fatalf("send: %v %+v", err, res)
	}
	if len(got) != 1 || got[0].Account != "priya@example.com" || got[0].Service != "gmail" || got[0].Text != "Signed and sent." {
		t.Errorf("recorded %+v", got)
	}
}

func TestEmailsSentAsTheOperatorCountAsOutbound(t *testing.T) {
	for _, n := range []string{"gmail.reply", "gmail.send"} {
		if !outboundTools[tools.CanonicalName(n)] {
			t.Errorf("%s reaches another person and is not counted as outbound", n)
		}
	}
	if outboundTools[tools.CanonicalName("gmail.label")] {
		t.Error("labelling reaches nobody")
	}
}
