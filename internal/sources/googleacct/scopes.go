package googleacct

import "strings"

// What a login may do in Gmail, as services a call asks for (Login.Can).
//
// Gmail's scopes are not one switch: gmail.metadata reads headers only,
// readonly reads everything, modify adds labels and sending, and send or
// compose send without reading. Each call asks for the power it needs, so a
// login without it is passed over and the fix names the scope to add.
const (
	GmailRead   = "gmail"        // list new mail and read its headers and snippet
	GmailBody   = "gmail.body"   // read whole messages and threads
	GmailModify = "gmail.modify" // add and remove labels: archive, mark read
	GmailSend   = "gmail.send"   // send mail as the account
)

const scopePrefix = "https://www.googleapis.com/auth/"

// gmailPowers are the Gmail powers a set of granted scopes gives.
func gmailPowers(scopes []string) []string {
	var read, body, modify, send bool
	for _, s := range scopes {
		switch strings.TrimSuffix(strings.TrimPrefix(s, scopePrefix), "/") {
		case "https://mail.google.com", "gmail.modify":
			read, body, modify, send = true, true, true, true
		case "gmail.readonly":
			read, body = true, true
		case "gmail.metadata":
			read = true
		case "gmail.send", "gmail.compose":
			send = true
		}
	}
	var out []string
	for _, p := range []struct {
		on   bool
		name string
	}{{read, GmailRead}, {body, GmailBody}, {modify, GmailModify}, {send, GmailSend}} {
		if p.on {
			out = append(out, p.name)
		}
	}
	return out
}

// serviceScopes are the scopes that grant a service, as a fix asks for them.
// Every Gmail power is asked for as gmail.modify: reading, labels and sending
// in one scope, which is what acting freely on mail needs.
var serviceScopes = map[string][]string{
	"calendar": {scopePrefix + "calendar"},
	"chat": {scopePrefix + "chat.spaces", scopePrefix + "chat.messages",
		scopePrefix + "chat.memberships", scopePrefix + "chat.users.readstate"},
	GmailRead:   {scopePrefix + "gmail.modify"},
	GmailBody:   {scopePrefix + "gmail.modify"},
	GmailModify: {scopePrefix + "gmail.modify"},
	GmailSend:   {scopePrefix + "gmail.modify"},
}

// scopesWith is a login's scopes with those service needs added: the list a
// fresh sign-in asks for so that it loses nothing. The identity scopes are
// left out because gws adds them itself, and narrower Gmail scopes give way
// to gmail.modify.
func scopesWith(held []string, service string) []string {
	add := serviceScopes[service]
	gmail := strings.HasPrefix(service, "gmail")
	var out []string
	for _, s := range held {
		short := strings.TrimPrefix(s, scopePrefix)
		switch {
		case s == "openid", s == "email", s == "profile", strings.HasPrefix(short, "userinfo."):
			continue
		case gmail && (short == "gmail.readonly" || short == "gmail.metadata"):
			continue
		}
		out = appendOnce(out, s)
	}
	for _, s := range add {
		out = appendOnce(out, s)
	}
	return out
}

// gogService is the name gog's `auth add --services` knows a service by, or
// "" for one it does not.
func gogService(service string) string {
	switch {
	case service == "calendar", service == "chat":
		return service
	case strings.HasPrefix(service, "gmail"):
		return "gmail"
	}
	return ""
}
