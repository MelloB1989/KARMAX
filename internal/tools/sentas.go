package tools

import "strings"

// Acting as the operator, told after.
//
// The operator lets KARMAX write to people as them — a Slack or Google Chat
// message, a GitHub comment or review, an answer to an invitation — and is
// told afterwards what was sent. That telling must not depend on the model
// remembering to mention it, so every tool that acts as the operator says
// what it did, in one shape, to one recorder; the runtime's recorder keeps it
// where every send as the operator is kept (the outbound ledger the agent's
// per-turn "Sent as you" report reads) and in the operator's app.

// SentAs is one thing said or done to other people as the operator, through
// one of the operator's own accounts.
type SentAs struct {
	// Service is where it happened, by the source's name: "slack", "gchat",
	// "gcal", "github", …
	Service string
	// Account is the operator's login it was done as: a workspace, a Google
	// address, a GitHub login.
	Account string
	// To is where it went, as the operator would say it: a person, a
	// channel, an issue, an invitation.
	To string
	// Action is what was done when it was more than saying something:
	// "approved", "accepted", "merged". Empty for a plain message.
	Action string
	// Text is what was said, as the people on the other end read it.
	Text string
	URL  string
}

// Content is the act in one line: what was done, then what was said.
func (s SentAs) Content() string {
	said := strings.Join(strings.Fields(s.Text), " ")
	action := strings.TrimSpace(s.Action)
	switch {
	case action == "":
		return said
	case said == "":
		return action
	}
	return action + ": " + said
}

// SentRecorder hears every SentAs. Each acting tool takes one and calls it
// once its act has succeeded. A nil recorder records nothing.
type SentRecorder func(SentAs)

// Record tells r, if there is one.
func (r SentRecorder) Record(s SentAs) {
	if r != nil {
		r(s)
	}
}

// SendsAsOperator is a registered tool that acts as the operator and records
// what it sends through the recorder it is given.
type SendsAsOperator interface {
	Tool
	RecordSentWith(SentRecorder)
}

// GiveSentRecorder hands rec to every registered tool that sends as the
// operator, and reports how many there were. The runtime calls it once its
// recorder exists; a tool lent per turn is built with the recorder instead.
func (r *Registry) GiveSentRecorder(rec SentRecorder) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	given := map[Tool]bool{}
	for _, t := range r.tools {
		if s, ok := t.(SendsAsOperator); ok && !given[t] {
			s.RecordSentWith(rec)
			given[t] = true
		}
	}
	return len(given)
}

// serviceNames are the services whose names are not their source name
// capitalised.
var serviceNames = map[string]string{
	"whatsapp": "WhatsApp",
	"github":   "GitHub",
	"gchat":    "Google Chat",
	"gcal":     "Google Calendar",
	"posthog":  "PostHog",
	"aws":      "AWS",
}

// ServiceName is a service as the operator reads it: "Google Chat" for
// "gchat", "Slack" for "slack".
func ServiceName(service string) string {
	s := strings.TrimSpace(service)
	if n, ok := serviceNames[strings.ToLower(s)]; ok {
		return n
	}
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
