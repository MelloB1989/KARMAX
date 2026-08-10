//go:build wasip1

// Package coldscan is the cold-memory pipeline as a marketplace loop: it walks
// OLDER WhatsApp chats (ones the operator is no longer actively texting in) and
// distills a durable per-chat summary — via Kit.Summarize (the agent's cheap
// summary model) — into the chat-summary store the memory retrieval sub-agent
// reads. Hot/active chats are left to hot-sync; large community/promo groups
// the operator barely participates in are skipped.
//
// Config (all optional, via KARMAX_LOOP_COLD_SCAN_*):
//
//	PER_TICK            chats summarized per run (default 3)
//	HOT_DAYS            operator-activity window that keeps a chat "hot" (default 14)
//	MIN_GROUP_OWN       min own messages for a group to matter (default 5)
//	MIN_GROUP_OWN_RATIO min own-message fraction for a group (default 0.2)
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/loopwasm"
)

const summaryPrompt = `You write durable memory about one of the operator's contacts based on a WhatsApp conversation. Summarize: who the other party is (relationship/role if inferable), the key topics discussed, any commitments / decisions / deadlines / important facts, and anything genuinely useful to remember later. 2–6 factual sentences, no fluff. If the conversation has no substance worth remembering (spam, one-off, pure logistics with no lasting info), reply with exactly: SKIP`

// recheckInterval bounds how often a chat is re-examined (one wacli own-message
// lookup per chat per day), keeping the loop cheap across hundreds of chats.
const recheckInterval = 24 * time.Hour

type chatRec struct {
	JID           string    `json:"jid"`
	Name          string    `json:"name"`
	IsGroup       bool      `json:"is_group"`
	Locked        bool      `json:"locked"`
	LastMessageAt time.Time `json:"last_message_at"`
}

type msgRec struct {
	Content   string    `json:"content"`
	IsFromMe  bool      `json:"is_from_me"`
	Timestamp time.Time `json:"timestamp"`
}

// run examines chats and summarizes the "cold" ones. Hot vs cold is decided by
// the OPERATOR's own last message (not the chat's activity), so a group that
// stays busy with other people but that the operator hasn't texted in for weeks
// correctly becomes cold. Each chat is recorded (summarized | hot | skipped) so
// subsequent runs skip it cheaply until recheckInterval elapses.
//
//go:wasmexport run
func run() {
	if err := scan(); err != nil {
		loopwasm.Log("cold-scan: %v", err)
	}
}

func scan() error {
	// No wacli path any more: the chat and message reads are host functions, so
	// this loop never names the binary. The lookup survived the port as dead
	// code and asked for a host function the manifest does not declare.
	perTick := configInt("per_tick", 3)
	hotDays := configInt("hot_days", 14)
	minGroupOwn := configInt("min_group_own", 5)
	minGroupOwnRatio := configFloat("min_group_own_ratio", 0.2)

	chats, err := listChats()
	if err != nil {
		return fmt.Errorf("cold-scan: list chats: %w", err)
	}
	cutoff := time.Now().AddDate(0, 0, -hotDays)
	// Oldest chats first so genuinely-cold conversations get summarized promptly.
	sort.Slice(chats, func(i, j int) bool { return chats[i].LastMessageAt.Before(chats[j].LastMessageAt) })

	summarized, examined := 0, 0
	checkBudget := perTick * 8
	if checkBudget < 30 {
		checkBudget = 30
	}

	for _, c := range chats {
		// The run deadline is the host's now: KARMAX cancels the module when
		// the loop timeout passes, so there is no ctx to check here.
		if summarized >= perTick || examined >= checkBudget {
			break
		}
		// Note: "locked" chats are NOT skipped — with wacli access control
		// unconfigured every chat defaults to locked yet reads work fine.
		// Relevance is decided by the operator's participation below.
		ex, _ := loopwasm.GetChatSummary(c.JID)
		if ex != nil && time.Since(time.Unix(ex.SummarizedAt, 0)) < recheckInterval {
			continue // examined recently
		}

		ownLast, ownCount := ownLastMessage(c.JID)
		examined++

		record := func(status, summary string, msgCount int) {
			if err := loopwasm.SaveChatSummary(loopwasm.ChatSummary{
				ChatJID: c.JID, ChatName: c.Name, IsGroup: c.IsGroup,
				Summary: summary, MessageCount: msgCount, OwnMessageCount: ownCount,
				LastMessageAt: ownLast.Unix(), SummarizedAt: time.Now().Unix(), Status: status,
			}); err != nil {
				loopwasm.Log("cold-scan: store state failed for %s: %v", c.Name, err)
			}
		}

		// No / negligible participation -> not useful memory.
		if ownCount == 0 || (c.IsGroup && ownCount < minGroupOwn) {
			record("skipped", "", 0)
			continue
		}
		// Operator still active here -> hot; leave it to hot-sync.
		if ownLast.After(cutoff) {
			record("hot", "", 0)
			continue
		}
		// Cold, but don't re-summarize if nothing changed since last time.
		if ex != nil && ex.Status == "summarized" && !ownLast.After(time.Unix(ex.LastMessageAt, 0)) {
			record("summarized", ex.Summary, ex.MessageCount)
			continue
		}
		msgs := fetchMessages(c.JID, 150)
		if len(msgs) < 3 {
			record("skipped", "", len(msgs))
			continue
		}
		// Community/broadcast group filter: if the operator's messages are only
		// a small fraction of recent activity, it's a group they don't really
		// converse in (promo/announcement groups) — skip it.
		if c.IsGroup {
			own := 0
			for _, m := range msgs {
				if m.IsFromMe {
					own++
				}
			}
			if float64(own)/float64(len(msgs)) < minGroupOwnRatio {
				record("skipped", "", len(msgs))
				continue
			}
		}
		summary, ok := summarize(c, msgs)
		if !ok {
			record("skipped", "", len(msgs))
			continue
		}
		record("summarized", summary, len(msgs))
		summarized++
	}
	if summarized > 0 || examined > 0 {
		loopwasm.Log("cold-scan: summarized %d chats (examined %d)", summarized, examined)
	}
	return nil
}

func summarize(c chatRec, msgs []msgRec) (string, bool) {
	var b strings.Builder
	for _, m := range msgs {
		txt := strings.TrimSpace(strings.ReplaceAll(m.Content, "\n", " "))
		if txt == "" {
			continue
		}
		who := "them"
		if m.IsFromMe {
			who = "me"
		}
		if len(txt) > 220 {
			txt = txt[:220] + "…"
		}
		b.WriteString(who + ": " + txt + "\n")
	}
	transcript := strings.TrimSpace(b.String())
	if transcript == "" {
		return "", false
	}
	kind := "direct chat"
	if c.IsGroup {
		kind = "group"
	}
	resp, err := loopwasm.Summarize(summaryPrompt +
		fmt.Sprintf("\n\nConversation with %q (%s). Recent messages (\"me\" = the operator):\n\n%s", c.Name, kind, transcript))
	if err != nil {
		loopwasm.Log("cold-scan: summarize failed for %s: %v", c.Name, err)
		return "", false
	}
	resp = strings.TrimSpace(resp)
	if resp == "" || strings.EqualFold(resp, "SKIP") {
		return "", false
	}
	return resp, true
}

func listChats() ([]chatRec, error) {
	raw, err := loopwasm.Tool("whatsapp.chats", map[string]any{"limit": 1000})
	if err != nil {
		return nil, err
	}
	out := []byte(raw)
	var chats []chatRec
	if err := json.Unmarshal(out, &chats); err != nil {
		return nil, err
	}
	return chats, nil
}

// ownLastMessage returns the operator's most recent own-message time in a chat
// and a count of their recent own messages (capped by the lookup limit).
func ownLastMessage(jid string) (time.Time, int) {
	msgs := runMessages(jid, 50, true)
	var last time.Time
	for _, m := range msgs {
		if m.Timestamp.After(last) {
			last = m.Timestamp
		}
	}
	return last, len(msgs)
}

func fetchMessages(jid string, limit int) []msgRec {
	return runMessages(jid, limit, false)
}

func runMessages(jid string, limit int, fromMeOnly bool) []msgRec {
	raw, err := loopwasm.Tool("whatsapp.messages", map[string]any{
		"chat": jid, "limit": limit, "from_me_only": fromMeOnly})
	if err != nil {
		return nil
	}
	return parseMessages([]byte(raw))
}

// parseMessages handles both {"messages":[...]} and a bare [...] array.
func parseMessages(out []byte) []msgRec {
	var wrap struct {
		Messages []msgRec `json:"messages"`
	}
	if json.Unmarshal(out, &wrap) == nil && len(wrap.Messages) > 0 {
		return wrap.Messages
	}
	var arr []msgRec
	if json.Unmarshal(out, &arr) == nil {
		return arr
	}
	return nil
}

func configInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(loopwasm.Config(key))); err == nil && v > 0 {
		return v
	}
	return def
}

func configFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(loopwasm.Config(key)), 64); err == nil && v > 0 {
		return v
	}
	return def
}

func main() {}
