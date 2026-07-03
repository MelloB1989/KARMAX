package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// chat-sweep: the proactive-proxy's counterpart for the BACKLOG. The webhook
// proxy only reacts to NEW incoming messages; this sweep periodically reviews
// the monitored chats for items already pending — an unanswered question, a
// promised action, an approaching deadline — and acts on the operator's behalf
// (or flags a decision for approval). Runs via the Claude harness sub-agent.
var sweepMu sync.Mutex

func init() {
	loopkit.Register(loopkit.Loop{
		Name:        "chat-sweep",
		Description: "Reviews the monitored WhatsApp chats for pending items (unanswered questions, promised actions, deadlines) and proactively acts on the operator's behalf or flags decisions for approval.",
		Schedule:    loopkit.Every("4h"),
		Run:         runChatSweep,
	})
}

func runChatSweep(ctx context.Context, k loopkit.Kit) error {
	if !sweepMu.TryLock() {
		k.Logf("chat-sweep already running; skipping")
		return nil
	}
	defer sweepMu.Unlock()

	chats, err := monitoredChats(ctx, k)
	if err != nil {
		return fmt.Errorf("chat-sweep: list monitored chats: %w", err)
	}
	if len(chats) == 0 {
		k.Logf("chat-sweep: no monitored chats (beyond operator's own); nothing to do")
		return nil
	}
	if len(chats) > 10 {
		chats = chats[:10]
	}

	wacli := strings.TrimSpace(k.Config("wacli"))
	if wacli == "" {
		wacli = hostpaths.Wacli()
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
		"4. If it's a real DECISION, commitment, money, or anything sensitive/ambiguous: do NOT send — flag it.\n\n" +
		"Output EXACTLY one line per chat, no other text:\n" +
		"ACTED <chat name>: <what you sent/did>\n" +
		"APPROVE <chat name>: <the pending item + your suggested reply>\n" +
		"SKIP <chat name>: <why nothing is needed>"

	out, err := k.Harness(ctx, prompt)
	if err != nil {
		return fmt.Errorf("chat-sweep: harness: %w", err)
	}
	if looksLikeError(out) {
		return fmt.Errorf("chat-sweep: harness returned error/refusal: %.120s", out)
	}

	var acted, approve []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "ACTED"):
			acted = append(acted, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(up, "APPROVE"):
			approve = append(approve, strings.TrimSpace(line[7:]))
		}
	}
	k.Logf("chat-sweep: %d chats reviewed — %d acted, %d need approval", len(chats), len(acted), len(approve))

	if len(acted) > 0 {
		_ = k.Notify("✅ Handled while sweeping", "• "+strings.Join(acted, "\n• "))
	}
	proposeItems(k, "Flagged by the chat-sweep loop while reviewing monitored WhatsApp chats.", approve)
	return nil
}

// monitoredChats returns the chats KARMAX watches (from the wacli webhook
// scope), excluding the operator's own command chats.
func monitoredChats(ctx context.Context, k loopkit.Kit) ([]string, error) {
	body, status, err := k.HTTP(ctx, "GET", hostpaths.WacliAPIURL()+"/webhooks", nil, "")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("wacli /webhooks: HTTP %d", status)
	}
	var resp struct {
		Webhooks []struct {
			URL      string   `json:"url"`
			ChatJIDs []string `json:"chat_jids"`
			Enabled  bool     `json:"enabled"`
		} `json:"webhooks"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, err
	}
	operator := operatorChatSet()
	var out []string
	for _, wh := range resp.Webhooks {
		if !wh.Enabled || !strings.Contains(wh.URL, "/comms/whatsapp") {
			continue
		}
		for _, c := range wh.ChatJIDs {
			if !operator[normalizeChatID(c)] {
				out = append(out, c)
			}
		}
	}
	return out, nil
}
