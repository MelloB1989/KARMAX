package runtime

import (
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/identity"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

func kitFor(d *identity.Directory, trigger loopkit.Trigger) *loopKit {
	return &loopKit{loopName: "wa-monitor", rt: &KarmaxRuntime{who: d}, trigger: trigger}
}

// The incident, reproduced at the host: the monitor's own prompt named the
// sender after the group and carried a stored note that the operator's account
// was somebody else. A loop's words cannot be trusted to get this right, so
// the host puts the account's answer first, ahead of anything the loop says.
func TestALoopPromptLeadsWithWhoSentTheMessage(t *testing.T) {
	d := identity.New("Priya", []string{"15550001111", "123456789@lid"})
	k := kitFor(d, loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: map[string]any{
		"event_kind": "comms.message", "sender_jid": "123456789:4@lid", "sender_id": "123456789",
		"sender_label": "Omar", "sender_name": "Contoso founders",
	}})
	loopPrompt := "Message sender: Contoso founders (id 123456789)\n" +
		"This person has ALREADY IDENTIFIED YOU (— and this is a message from Omar, not Priya.)"

	for name, got := range map[string]string{
		"gateway": k.withIdentity(loopPrompt),
		"summary": k.summaryPrompt(loopPrompt),
	} {
		if !strings.HasPrefix(got, "Who is speaking") {
			t.Errorf("%s: the account's answer does not come first:\n%s", name, got)
		}
		if !strings.Contains(got, "sent by your operator, Priya") {
			t.Errorf("%s: the operator is not named as the sender:\n%s", name, got)
		}
		if !strings.HasSuffix(got, loopPrompt) {
			t.Errorf("%s: the loop's own prompt was altered:\n%s", name, got)
		}
	}
}

// Without a message behind the run there is no sender to name, but the loop is
// still told who the operator is — a sweep reads threads full of accounts.
func TestAScheduledRunIsToldWhoTheOperatorIs(t *testing.T) {
	d := identity.New("Priya", []string{"15550001111"})
	k := kitFor(d, loopkit.Trigger{Kind: loopkit.TriggerSchedule})
	got := k.withIdentity("sweep the chats")
	if !strings.Contains(got, "Your operator is Priya") || strings.Contains(got, "sent by") {
		t.Errorf("scheduled run preamble:\n%s", got)
	}
	// A summary on a schedule is often written for an audience — a post, a
	// digest — and gets nothing added.
	if got := k.summaryPrompt("write today's post"); got != "write today's post" {
		t.Errorf("a scheduled summary was given the operator's accounts:\n%s", got)
	}
}

// A signed loop — the WhatsApp monitor is one — reaches the model through a kit
// built per call. That kit has to know the message behind the run, or the
// monitor, the one loop this matters most for, would be the one left without.
func TestASignedLoopsModelCallsKnowTheMessageBehindTheRun(t *testing.T) {
	d := identity.New("Priya", []string{"123456789@lid"})
	w := &wasmKit{rt: &KarmaxRuntime{cfg: &config.KarmaxConfig{}, who: d}, loop: "wa-monitor"}
	w.setTrigger(loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: map[string]any{
		"event_kind": "comms.message", "sender_jid": "123456789:4@lid",
	}})
	if got := w.base().withIdentity("p"); !strings.Contains(got, "sent by your operator, Priya") {
		t.Errorf("a signed loop's model call was not told who sent the message:\n%s", got)
	}
	w.setTrigger(loopkit.Trigger{Kind: loopkit.TriggerSchedule})
	if got := w.base().withIdentity("p"); strings.Contains(got, "sent by") {
		t.Errorf("the next run kept the previous run's sender:\n%s", got)
	}
}

// Nothing configured, nothing added.
func TestNoDirectoryLeavesTheLoopPromptAlone(t *testing.T) {
	k := kitFor(nil, loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: map[string]any{"sender_jid": "1@lid"}})
	if got := k.withIdentity("p"); got != "p" {
		t.Errorf("got %q", got)
	}
	var nilKit *loopKit
	if got := nilKit.withIdentity("p"); got != "p" {
		t.Errorf("nil kit: %q", got)
	}
}
