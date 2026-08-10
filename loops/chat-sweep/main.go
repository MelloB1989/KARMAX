//go:build wasip1

// chat-sweep reviews the monitored WhatsApp chats for items already pending —
// an unanswered question, a promised action, an approaching deadline — and acts
// on the operator's behalf, or flags a decision for approval.
//
// It is the backlog counterpart to the webhook proxy, which only sees NEW
// messages. Ported from the compiled-in loop of the same name.
package main

import (
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/loops/internal/shared"
	"github.com/MelloB1989/karmax/pkg/loopwasm"
)

// maxChats bounds one sweep. Reviewing everything in one harness call produces
// a worse review of each, and a longer one.
const maxChats = 10

//go:wasmexport run
func run() {
	if err := sweep(); err != nil {
		loopwasm.Log("chat-sweep: %v", err)
	}
}

func sweep() error {
	// Single-flight came from a mutex in the compiled-in version. It is not
	// needed here: KARMAX takes a lease per loop, so a run that is still going
	// means this one is never started.
	chats, err := shared.MonitoredChats()
	if err != nil {
		return fmt.Errorf("listing monitored chats: %w", err)
	}
	if len(chats) == 0 {
		loopwasm.Log("chat-sweep: no monitored chats beyond the operator's own; nothing to do")
		return nil
	}
	if len(chats) > maxChats {
		chats = chats[:maxChats]
	}

	wacli := strings.TrimSpace(loopwasm.Config("wacli"))
	if wacli == "" {
		wacli = loopwasm.HostTool("wacli")
	}

	var list strings.Builder
	for _, c := range chats {
		fmt.Fprintf(&list, "- %s\n", c)
	}

	prompt := "You are the operator's proactive WhatsApp assistant, managing their account via the wacli CLI at " + wacli + ".\n\n" +
		"Review each of these monitored chats for PENDING items:\n" + list.String() + "\n" +
		"For EACH chat:\n" +
		fmt.Sprintf("1. Run: %s messages --chat \"<jid>\" --limit 20   (oldest-first; is_from_me=true is the operator)\n", wacli) +
		"2. Determine whether something is pending on the OPERATOR'S side: an unanswered question to them, something they promised and haven't delivered, a deadline near, a follow-up they owe. Already-resolved threads or ones simply awaiting the OTHER person are NOT pending.\n" +
		"3. If pending and ROUTINE (acknowledgement, confirming availability, a simple follow-up nudge, sharing already-known info) and you're confident how the operator would respond: act NOW — send with `" + wacli + " send --to \"<jid>\" --text \"...\"` in the operator's natural, human voice (concise; never reveal you're an AI).\n" +
		"4. If it's a real DECISION, commitment, money, or anything sensitive/ambiguous: do NOT send — flag it as APPROVE.\n" +
		"5. If the pending item is something ONLY the operator can personally do (send a document/file you don't have, a personal/family reply, an offline task): flag it as REMIND.\n\n" +
		shared.ScanOutputSpec

	out, err := loopwasm.Harness(prompt)
	if err != nil {
		return fmt.Errorf("harness: %w", err)
	}
	if shared.LooksLikeError(out) {
		return fmt.Errorf("the harness refused or errored: %.120s", out)
	}

	acted, approve, remind, inform := shared.ParseScanOutcomes(out)
	loopwasm.Log("chat-sweep: %d chats reviewed — %d acted, %d need approval, %d reminders, %d fyi",
		len(chats), len(acted), len(approve), len(remind), len(inform))

	if len(acted) > 0 {
		_ = loopwasm.Notify("✅ Handled while sweeping", "• "+strings.Join(acted, "\n• "))
	}
	shared.ProposeItems("Flagged by the chat-sweep loop while reviewing monitored WhatsApp chats.", approve)
	shared.RemindItems("Flagged by the chat-sweep loop: only you can do this one.", remind)
	shared.InformItems("📣 Update from your chats", inform)
	return nil
}

func main() {}
