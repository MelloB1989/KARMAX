package core

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// act-on-pending: drains the pending-actions queue (filled by scans like
// memory-bootstrap) and, via the Claude harness executor, completes what it
// safely can — calendar/tasks via gws, WhatsApp replies only in MONITORED
// chats — and flags real decisions for the operator's approval.
var actPendingMu sync.Mutex

func init() {
	loopkit.Register(loopkit.Loop{
		Name:        "act-on-pending",
		Description: "Executes actionable items discovered by scans: completes what it safely can (calendar via gws, replies in monitored chats) and flags real decisions for approval.",
		Schedule:    loopkit.Every("2h"),
		Run:         runActOnPending,
	})
}

func runActOnPending(ctx context.Context, k loopkit.Kit) error {
	if !actPendingMu.TryLock() {
		k.Logf("act-on-pending already running; skipping")
		return nil
	}
	defer actPendingMu.Unlock()

	items, err := drainPending()
	if err != nil {
		return fmt.Errorf("act-on-pending: drain queue: %w", err)
	}
	if len(items) == 0 {
		k.Logf("act-on-pending: queue empty; nothing to do")
		return nil
	}
	// Bound a single tick so a huge backlog doesn't make one harness call
	// unwieldy; the rest stays queued for the next tick.
	const maxPerTick = 15
	if len(items) > maxPerTick {
		requeuePending(items[maxPerTick:])
		items = items[:maxPerTick]
	}

	wacli := strings.TrimSpace(k.Config("wacli"))
	if wacli == "" {
		wacli = hostpaths.Wacli()
	}
	gws := strings.TrimSpace(k.Config("gws"))
	if gws == "" {
		gws = hostpaths.GWS()
	}

	monitored, err := monitoredChats(ctx, k)
	if err != nil {
		k.Logf("act-on-pending: monitored chats lookup failed: %v", err)
	}
	monitoredList := "(none)"
	if len(monitored) > 0 {
		monitoredList = strings.Join(monitored, ", ")
	}

	prompt := "You are the operator's proactive assistant. These PENDING items were surfaced from their WhatsApp:\n\n- " +
		strings.Join(items, "\n- ") + "\n\n" +
		"Tools on this machine: the wacli CLI at " + wacli + " (WhatsApp: `messages --chat <jid> --limit N`, `send --to <jid> --text \"...\"`) and the gws CLI at " + gws + " (Google Workspace: calendar, gmail, tasks — run `gws calendar --help` etc. to discover syntax).\n\n" +
		"For EACH item:\n" +
		"1. Verify it is STILL open (re-read the chat if needed); many are old — anything already resolved, expired, or stale gets SKIP.\n" +
		"2. If you can COMPLETE it without messaging anyone (e.g. create a calendar event for an already-agreed meeting, add a task): DO IT NOW via gws.\n" +
		"3. If it needs a WhatsApp message AND the chat is in this MONITORED list: " + monitoredList + " — send it now in the operator's natural human voice (concise, never reveal you're an AI).\n" +
		"4. Anything else still relevant (a message in a non-monitored chat, a real decision, money, sensitive): do NOT act — flag it.\n\n" +
		"Output EXACTLY one line per item, no other text:\n" +
		"ACTED <who/what>: <what you did>\n" +
		"APPROVE <who/what>: <the open item + your suggested action>\n" +
		"SKIP <who/what>: <why>"

	out, err := k.Harness(ctx, prompt)
	if err != nil {
		requeuePending(items) // don't lose items on a transient failure
		return fmt.Errorf("act-on-pending: harness: %w", err)
	}
	if looksLikeError(out) {
		requeuePending(items)
		return fmt.Errorf("act-on-pending: harness returned error/refusal: %.120s", out)
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
	k.Logf("act-on-pending: %d items — %d acted, %d need approval", len(items), len(acted), len(approve))
	if len(acted) > 0 {
		_ = k.Notify("✅ Completed from scan", "• "+strings.Join(acted, "\n• "))
	}
	proposeItems(k, "Flagged by the act-on-pending loop from items discovered in WhatsApp scans.", approve)
	return nil
}
