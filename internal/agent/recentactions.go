package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/tools"
)

// What the agent has just done, put in front of it before it decides what to do
// next.
//
// Everything here is event-driven, and events repeat. A WhatsApp reply carries
// the message it quotes, so an operator replying to their own instruction
// re-delivers that instruction verbatim; a retry, a replay after a restart, or
// two loops reacting to one message all arrive as another perfectly ordinary
// request. Observed: "call Shiva and ask when Aymaan is starting" was carried
// out three times in thirty-two seconds, and Shiva received the same question
// three times in three wordings, each pass having independently decided it was
// the one handling it.
//
// No guard fixes that. A text comparison cannot catch three different wordings,
// and a rule against acting twice would be wrong the moment the operator really
// does want something said again. What every one of those passes lacked was a
// fact: it had already done this, moments ago, to that person. Given the fact,
// telling "I already did this" from "they want it again" is exactly the
// judgement the model is for — so this puts the fact in the context and leaves
// the judgement where it belongs.
const (
	// recentActionWindow is how far back an action stays worth mentioning.
	// Long enough to cover a duplicate event and a multi-pass turn; short
	// enough that yesterday's messages are not re-litigated on every prompt.
	recentActionWindow = 20 * time.Minute
	// maxRecentActions bounds the section. These are the newest few, not a
	// log: the point is recognition, and a page of history would cost more
	// context than it saves.
	maxRecentActions  = 8
	recentActionChars = 90
)

// buildRecentActionsContext renders the actions this agent has just taken.
func (a *Agent) buildRecentActionsContext() string {
	if a.store == nil {
		return ""
	}
	events, err := a.store.RecentLogEvents("", string(bus.EventToolCalled), 60)
	if err != nil || len(events) == 0 {
		return ""
	}
	cutoff := time.Now().Add(-recentActionWindow)

	type action struct {
		at   time.Time
		line string
	}
	var actions []action
	seen := map[string]bool{}
	for _, e := range events {
		if e.CreatedAt.Before(cutoff) || (e.AgentID != "" && e.AgentID != a.def.ID) {
			continue
		}
		name, _ := e.Payload["tool"].(string)
		if name == "" {
			continue
		}
		input, _ := e.Payload["input"].(map[string]any)
		line := describeAction(name, input)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		actions = append(actions, action{at: e.CreatedAt, line: line})
		if len(actions) >= maxRecentActions {
			break
		}
	}
	if len(actions) == 0 {
		return ""
	}
	// Oldest first, so the section reads as a sequence.
	sort.Slice(actions, func(i, j int) bool { return actions[i].at.Before(actions[j].at) })

	var b strings.Builder
	b.WriteString("\n## What you have JUST done (last 20 minutes)\n")
	b.WriteString("Check this before acting. If the request in front of you is something you have " +
		"already done below, do NOT do it again — say what you already did. A WhatsApp reply repeats " +
		"the message it quotes, so an instruction can arrive twice; the same instruction is not a " +
		"second request. Act again only if they are clearly asking for it again.\n")
	for _, act := range actions {
		b.WriteString(fmt.Sprintf("- %s ago: %s\n", compactSince(act.at), act.line))
	}
	return b.String()
}

// describeAction renders one tool call as the thing it did, in the terms the
// operator would use. Only the tools whose repetition is visible to somebody
// else are worth the context: sending, calling, posting, reminding.
func describeAction(name string, input map[string]any) string {
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := input[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	// Canonical form: names reach the log both ways ("comms.send" from the
	// manifest, "comms_send" from the model), and matching one spelling meant
	// matching neither reliably.
	switch tools.CanonicalName(name) {
	case "comms_send", "whatsapp_send_message":
		to, body := str("target", "to", "recipient", "chat"), str("content", "text", "message")
		if to == "" && body == "" {
			return ""
		}
		return fmt.Sprintf("messaged %s: %q", shortTarget(to), truncateAction(body))
	case "call_start", "whatsapp_place_call":
		if to := str("to", "target", "recipient"); to != "" {
			return "called " + shortTarget(to)
		}
	case "reminder_add", "self_remind", "scheduler_add":
		if what := str("title", "prompt", "task", "text"); what != "" {
			return fmt.Sprintf("set a reminder: %q", truncateAction(what))
		}
	case "linkedin_post", "x_post":
		if what := str("text", "content"); what != "" {
			return fmt.Sprintf("posted: %q", truncateAction(what))
		}
	}
	return ""
}

// shortTarget keeps a recipient recognisable without reading a JID aloud.
func shortTarget(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return "someone"
	}
	if i := strings.IndexAny(target, "@:"); i > 0 {
		target = target[:i]
	}
	return target
}

func truncateAction(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= recentActionChars {
		return s
	}
	return string([]rune(s)[:recentActionChars]) + "…"
}

// compactSince renders an age the way a person would say it.
func compactSince(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}
