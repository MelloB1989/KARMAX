package runtime

import (
	"strings"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// Identity on a loop's model calls.
//
// A loop writes its own prompt, and a loop's prompt can be wrong about who is
// speaking: the WhatsApp monitor named a group message's sender after the group
// and, in the same prompt, carried a note it had stored months earlier saying
// the operator's own account was a co-founder's. The model believed the note.
// The host cannot rewrite a signed loop, but every model call a loop makes
// passes through here, so the account's answer goes first — ahead of anything
// the loop says — with the instruction that whatever disagrees with it is wrong.

// withIdentity puts the authoritative who-is-who ahead of a loop's prompt —
// and, after it, what tasks already own in the run's chat (loopask.go), which
// is as much a fact the loop's prompt can be wrong about.
func (k *loopKit) withIdentity(prompt string) string {
	if k == nil || k.rt == nil {
		return prompt
	}
	if note := k.inFlightNote(); note != "" {
		prompt = note + "\n" + prompt
	}
	if k.rt.who == nil {
		return prompt
	}
	sender, label := triggerSender(k.trigger)
	pre := k.rt.who.Preamble(sender, label)
	if pre == "" {
		return prompt
	}
	return pre + "\n" + prompt
}

// summaryPrompt is withIdentity for a summary, which only a run about a message
// gets. A summary on a schedule is often written for an audience — a post, a
// digest — and the operator's accounts have no business in it.
func (k *loopKit) summaryPrompt(prompt string) string {
	if k == nil {
		return prompt
	}
	if sender, _ := triggerSender(k.trigger); sender == "" {
		return prompt
	}
	return k.withIdentity(prompt)
}

// triggerSender is who sent the message behind an event-triggered run: the
// account the channel attributed it to, and that account's label.
func triggerSender(t loopkit.Trigger) (sender, label string) {
	if t.Kind != loopkit.TriggerEvent || t.Payload == nil {
		return "", ""
	}
	str := func(key string) string {
		s, _ := t.Payload[key].(string)
		return strings.TrimSpace(s)
	}
	sender = str("sender_jid")
	if sender == "" {
		sender = str("sender_id")
	}
	return sender, str("sender_label")
}
