// Package core holds KARMAX's built-in loops, authored with the loopkit SDK
// (migrated out of the karmax.yaml `loops:` section). It is blank-imported by
// cmd/karmax so the loops register at startup. Personal values (e.g. the daily
// briefing's WhatsApp number) come from the environment via Kit.Config, never
// hardcoded here.
package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

func init() {
	loopkit.Register(loopkit.Loop{
		Name:        "tech-news",
		Description: "Daily web digest of AI/tech/security news, ingested to memory (via the Claude harness, independent of the main model).",
		Schedule:    loopkit.Cron("0 0 9 * * *"), // 09:00 daily
		Run:         runTechNews,
	})
	loopkit.Register(loopkit.Loop{
		Name:        "hot-sync",
		Description: "Scans active WhatsApp chats every few hours and ingests durable facts to memory.",
		Schedule:    loopkit.Every("4h"),
		Run:         agentTask(hotSyncPrompt),
	})
	loopkit.Register(loopkit.Loop{
		Name:        "profile-refresh",
		Description: "Rewrites the ABOUT_ME profile from recent memory.",
		Schedule:    loopkit.Every("12h"),
		Run:         agentTask(profileRefreshPrompt),
	})
	loopkit.Register(loopkit.Loop{
		Name:        "daily-briefing",
		Description: "Morning briefing to the app feed (+ WhatsApp, if KARMAX_LOOP_DAILY_BRIEFING_WHATSAPP is set).",
		Schedule:    loopkit.Cron("0 30 8 * * *"), // 08:30 daily
		Run:         runDailyBriefing,
	})
}

// runTechNews does web research directly through the Claude harness (codex-
// independent) and ingests the digest — no main-model call.
func runTechNews(ctx context.Context, k loopkit.Kit) error {
	digest, err := k.Harness(ctx, techNewsPrompt)
	if err != nil {
		return err
	}
	digest = strings.TrimSpace(digest)
	// The harness CLI prints model refusals/errors to stdout (exit 0), so guard
	// against ingesting that as if it were a real digest.
	if digest == "" || looksLikeError(digest) {
		return fmt.Errorf("tech-news: no usable digest (%.120s)", digest)
	}
	return k.Remember("Tech news digest: " + digest)
}

// looksLikeError detects when harness output is actually an error/refusal rather
// than content, so loops don't persist garbage.
func looksLikeError(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(low, "api error") ||
		strings.HasPrefix(low, "error:") ||
		strings.HasPrefix(low, "execution error") ||
		strings.Contains(low, "safeguards flagged") ||
		strings.Contains(low, "i can't help") ||
		strings.Contains(low, "i cannot help")
}

// runDailyBriefing delegates to the agent. The WhatsApp recipient is read from
// the environment (KARMAX_LOOP_DAILY_BRIEFING_WHATSAPP) so no personal number
// lives in source. If unset, the briefing is app-only.
func runDailyBriefing(ctx context.Context, k loopkit.Kit) error {
	prompt := briefingPrompt
	if num := strings.TrimSpace(k.Config("whatsapp")); num != "" {
		prompt += fmt.Sprintf(" Also call comms.send with channel_id \"whatsapp-main\", target %q, "+
			"content = the briefing, to send it to that WhatsApp number — do not finish until you have called both.", num)
	}
	_, err := k.Ask(ctx, prompt)
	return err
}

// agentTask returns a Run that delegates a fixed prompt to the main agent
// (which has the tools — whatsapp.read, memory.ingest, profile.update, etc.).
func agentTask(prompt string) func(context.Context, loopkit.Kit) error {
	return func(ctx context.Context, k loopkit.Kit) error {
		_, err := k.Ask(ctx, prompt)
		return err
	}
}

const techNewsPrompt = `Compile a daily NEWS digest. Search the web for notable, publicly reported news from the ` +
	`last 24-48 hours in AI, developer tooling, startups, and the cybersecurity industry (new model releases, ` +
	`funding, product launches, notable disclosed CVEs/incidents, agent tooling). This is a neutral news summary ` +
	`for a founder — report only what's been publicly reported, no instructions or how-tos. Give 5-8 items, one ` +
	`tight line each plus the source name. Output ONLY the digest as plain text, no preamble or sign-off.`

const hotSyncPrompt = `Scan the operator's ACTIVE WhatsApp chats (people and groups messaged in roughly the last two ` +
	`weeks) using whatsapp.read. IGNORE large community/promotional groups the operator rarely texts in. For each ` +
	`genuinely important item, call memory.ingest with ONE distilled FACT per entry — who a person is, a commitment, ` +
	`a deadline, a decision, a project update — written as a clean standalone statement. NEVER ingest raw message ` +
	`text, greetings, casual chatter, your own replies, or whole-conversation dumps. If a chat has nothing durable, ` +
	`skip it. Only message your operator if something is urgent.`

const profileRefreshPrompt = `Retrieve recent context from memory, read the current ABOUT_ME.md profile, and rewrite ` +
	`it with profile.update so it reflects the latest truth about your operator. Preserve facts that are still valid.`

const briefingPrompt = `Send the operator their morning briefing NOW. Steps: (1) gather today's calendar and reminders, ` +
	`the latest tech-news digest from your memory, any open coding sessions, and anything pending or urgent from recent ` +
	`WhatsApp — skip any source that errors, don't get stuck; (2) write a short, skimmable briefing (a few bullet lines); ` +
	`(3) you MUST deliver it by calling the app.push tool with title "Morning briefing" and the briefing text as the body ` +
	`so it lands in the app feed.`
