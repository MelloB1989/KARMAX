package whatsapp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/identity"
	"go.uber.org/zap"
)

// The operator configures a phone number and a LID; KARMAX is linked to a
// different account.
func channelFor(t *testing.T) *WhatsAppChannel {
	t.Helper()
	w := New("wa", "/nonexistent/wacli", "", "", zap.NewNop())
	d := identity.New("Priya", []string{"15550001111", "123456789@lid"})
	d.SetLinked("15550002222:63@s.whatsapp.net", "987654321@lid")
	w.SetIdentity(d)
	return w
}

func route(t *testing.T, w *WhatsAppChannel, body string) comms.Message {
	t.Helper()
	var env wacliWebhookEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	w.routeEvent(env)
	select {
	case m := <-w.inbox:
		return m
	case <-time.After(time.Second):
		t.Fatal("nothing was routed")
	}
	return comms.Message{}
}

// The incident: the operator wrote in a group from their own account, and the
// message reached the monitor named after the GROUP — "Message sender: <group
// name>" — beside a note that called the operator somebody else. The sender of
// a message is a person, and when that person is the operator it says so.
func TestAGroupMessageFromTheOperatorIsNamedAndMarkedAsTheOperator(t *testing.T) {
	w := channelFor(t)
	m := route(t, w, `{"event":"incoming_message","payload":{
		"chat":{"jid":"120363000000000001@g.us","name":"Contoso founders","is_group":true},
		"message":{"id":"m1","chat_jid":"120363000000000001@g.us","sender_jid":"123456789:4@lid","content":"keep it at 2pm"},
		"sender_contact":{"jid":"123456789@lid","full_name":"Omar","push_name":"Omar"},
		"source":"whatsapp_event"}}`)

	if m.SenderName != "Priya" {
		t.Errorf("SenderName = %q, want the operator's configured name (not the group, not a label)", m.SenderName)
	}
	if m.Metadata["sender_role"] != "operator" || m.Metadata["sender_is_operator"] != true {
		t.Errorf("not marked as the operator: %v", m.Metadata)
	}
	if m.Metadata["chat_name"] != "Contoso founders" {
		t.Errorf("chat_name = %v; the chat keeps its own name", m.Metadata["chat_name"])
	}
	if m.Metadata["sender_jid"] != "123456789:4@lid" {
		t.Errorf("sender_jid = %v", m.Metadata["sender_jid"])
	}
}

// Anyone else in a group is named by their own contact record — never the
// group's name, which is what every member used to be called.
func TestAGroupMessageFromSomeoneElseCarriesTheirOwnName(t *testing.T) {
	w := channelFor(t)
	m := route(t, w, `{"event":"incoming_message","payload":{
		"chat":{"jid":"120363000000000001@g.us","name":"Contoso founders","is_group":true},
		"message":{"id":"m2","sender_jid":"555000444@lid","content":"is it 2pm?"},
		"sender_contact":{"jid":"555000444@lid","full_name":"","push_name":"Omar"},
		"source":"whatsapp_event"}}`)

	if m.SenderName != "Omar" {
		t.Errorf("SenderName = %q, want the sender's own name", m.SenderName)
	}
	if m.Metadata["sender_role"] != "contact" || m.Metadata["sender_is_operator"] != false {
		t.Errorf("a third party was not marked as one: %v", m.Metadata)
	}
}

// Without a contact record (a replayed message carries none) the name is looked
// up, and failing that the sender is unnamed — still never the group.
func TestAnUnknownGroupMemberIsLookedUpNotNamedAfterTheGroup(t *testing.T) {
	w := channelFor(t)
	body := `{"event":"incoming_message","payload":{
		"chat":{"jid":"120363000000000001@g.us","name":"Contoso founders","is_group":true},
		"message":{"id":"m3","sender_jid":"555000444@lid","content":"hello"},
		"source":"whatsapp_event"}}`

	if m := route(t, w, body); m.SenderName == "Contoso founders" || m.SenderName != "an unnamed contact (555000444)" {
		t.Errorf("SenderName = %q", m.SenderName)
	}

	w.SetNames(func(jid string) string {
		if identity.Bare(jid) == "555000444" {
			return "Omar"
		}
		return ""
	})
	if m := route(t, w, body); m.SenderName != "Omar" {
		t.Errorf("SenderName = %q, want the looked-up name", m.SenderName)
	}
}

// A direct chat is named after the person in it, so its name is their label.
func TestADirectChatFallsBackToTheChatName(t *testing.T) {
	w := channelFor(t)
	m := route(t, w, `{"event":"incoming_message","payload":{
		"chat":{"jid":"15550003333@s.whatsapp.net","name":"Omar"},
		"message":{"id":"m4","sender_jid":"15550003333@s.whatsapp.net","content":"hi"},
		"source":"whatsapp_event"}}`)
	if m.SenderName != "Omar" || m.Metadata["sender_role"] != "contact" {
		t.Errorf("direct chat sender = %q (%v)", m.SenderName, m.Metadata["sender_role"])
	}
}

// The operator's own direct chat teaches the LID they speak under elsewhere.
//
// Configured by phone number alone, the operator spoke in groups under a LID
// nothing knew was theirs. A message in their own direct chat can only be from
// them, so the id it carries is theirs, and the next group message from it is
// theirs too.
func TestTheOperatorsDirectChatTeachesTheLIDTheySpeakUnderInGroups(t *testing.T) {
	w := New("wa", "/nonexistent/wacli", "", "", zap.NewNop())
	w.SetIdentity(identity.New("Priya", []string{"15550001111"}))

	route(t, w, `{"event":"incoming_message","payload":{
		"chat":{"jid":"15550001111@s.whatsapp.net","name":"Priya"},
		"message":{"id":"m5","sender_jid":"123456789@lid","content":"status?"},
		"source":"whatsapp_event"}}`)

	m := route(t, w, `{"event":"incoming_message","payload":{
		"chat":{"jid":"120363000000000001@g.us","name":"Contoso founders","is_group":true},
		"message":{"id":"m6","sender_jid":"123456789:2@lid","content":"on my way"},
		"sender_contact":{"push_name":"Omar"},
		"source":"whatsapp_event"}}`)
	if m.Metadata["sender_role"] != "operator" || m.SenderName != "Priya" {
		t.Errorf("the operator's LID was not learned: %q %v", m.SenderName, m.Metadata["sender_role"])
	}
}

// A call from the operator is a call from the operator, whatever their account
// is labelled.
func TestACallIsNamedByWhoseAccountRings(t *testing.T) {
	w := channelFor(t)
	m := route(t, w, `{"event":"call.incoming","payload":{"call_id":"C1","peer_jid":"123456789@lid","peer_name":"Omar"}}`)
	if m.SenderName != "Priya" || m.Metadata["sender_role"] != "operator" {
		t.Errorf("call from the operator named %q (%v)", m.SenderName, m.Metadata["sender_role"])
	}
}
