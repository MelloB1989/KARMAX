package agent

import (
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/identity"
	"go.uber.org/zap"
)

// Every turn is told who the operator is, from configuration — not left to
// assemble it from a profile summary, memory and whatever the thread says.
func TestEveryTurnIsToldWhoTheOperatorIs(t *testing.T) {
	a := &Agent{log: zap.NewNop()}
	if got := a.buildIdentityContext(); got != "" {
		t.Errorf("no directory, but the turn was told %q", got)
	}

	d := identity.New("Priya", []string{"15550001111", "123456789@lid"})
	d.SetLinked("15550002222@s.whatsapp.net")
	a.SetIdentity(d)
	got := a.buildIdentityContext()
	for _, want := range []string{"Priya", "15550001111", "123456789@lid", "15550002222", "authoritative"} {
		if !strings.Contains(got, want) {
			t.Errorf("identity context lacks %q:\n%s", want, got)
		}
	}
}

func commsEvent(payload map[string]any) bus.Event {
	base := map[string]any{
		"content": "move the call to 2pm", "channel_id": "123456789@lid",
		"channel_type": "whatsapp", "chat_name": "Priya", "sender_id": "123456789",
	}
	for k, v := range payload {
		base[k] = v
	}
	return bus.Event{ID: "e1", Kind: "comms.message", AgentID: "a", Payload: base}
}

// A message from the operator's own account says so, plainly.
//
// The turn used to open "establish WHO sent this — it is not automatically your
// operator" with nothing but a number to establish it from, in the very chat
// that only the operator can write in. A model told to doubt the operator, and
// given nothing to settle the doubt with, settles it from memory.
func TestAnOperatorMessageSaysItIsFromTheOperator(t *testing.T) {
	prompt := buildPromptFromEvent(commsEvent(map[string]any{
		"sender_role": "operator", "sender_is_operator": true, "sender_name": "Priya",
		"sender_jid": "123456789@lid", "sender_label": "Omar",
	}), nil)
	if !strings.Contains(prompt, "YOUR OPERATOR, Priya") {
		t.Errorf("the operator's message is not attributed to them:\n%s", prompt)
	}
	if strings.Contains(prompt, "not automatically your operator") {
		t.Errorf("the operator's own message is still put in doubt:\n%s", prompt)
	}
	if strings.Contains(prompt, "Omar") {
		t.Errorf("a label on the operator's account reached the turn as a name:\n%s", prompt)
	}
}

// Anyone else is named, and named as not the operator.
func TestAThirdPartyMessageSaysItIsNotTheOperator(t *testing.T) {
	prompt := buildPromptFromEvent(commsEvent(map[string]any{
		"sender_role": "contact", "sender_is_operator": false, "sender_name": "Omar",
		"sender_jid": "15550003333@s.whatsapp.net", "sender_id": "15550003333",
	}), nil)
	if !strings.Contains(prompt, "Omar") || !strings.Contains(prompt, "NOT your operator") {
		t.Errorf("a third party's message is not marked as one:\n%s", prompt)
	}
}

// A channel that cannot say who sent a message keeps the old caution: there is
// genuinely nothing to go on.
func TestAnUnattributedMessageStillAsksWhoSentIt(t *testing.T) {
	prompt := buildPromptFromEvent(commsEvent(nil), nil)
	if !strings.Contains(prompt, "Establish WHO sent this") {
		t.Errorf("an unattributed message lost its caution:\n%s", prompt)
	}
}
