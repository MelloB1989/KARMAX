package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// memory-bootstrap: a deep, one-time scan of the operator's WhatsApp history
// that builds detailed long-term memory. The heavy reading/distillation runs in
// the Claude harness (a sub-agent with its own token budget — NOT the codex
// main model), in small batches with a checkpoint file, so it survives
// restarts and finishes on its own even while the operator sleeps.
const (
	bootstrapBatchSize   = 4   // chats per harness call
	bootstrapMaxBatches  = 8   // batches per loop tick (32 chats/tick)
	bootstrapActiveDays  = 120 // only scan chats active in this window
	bootstrapMsgsPerChat = 250
)

// bootstrapMu prevents a manual trigger overlapping the scheduled tick.
var bootstrapMu sync.Mutex

func init() {
	loopkit.Register(loopkit.Loop{
		Name:        "memory-bootstrap",
		Description: "Deep initial scan of WhatsApp history via the Claude harness sub-agent (batched + checkpointed); builds detailed long-term memory, then goes dormant. Safe to trigger manually.",
		Schedule:    loopkit.Every("6h"),
		Run:         runMemoryBootstrap,
	})
}

type bootstrapChat struct {
	JID           string `json:"jid"`
	Name          string `json:"name"`
	IsGroup       bool   `json:"is_group"`
	Locked        bool   `json:"locked"`
	LastMessageAt string `json:"last_message_at"`
}

func runMemoryBootstrap(ctx context.Context, k loopkit.Kit) error {
	if !bootstrapMu.TryLock() {
		k.Logf("memory-bootstrap already running; skipping this trigger")
		return nil
	}
	defer bootstrapMu.Unlock()

	ckptPath, donePath, err := bootstrapPaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(donePath); err == nil {
		k.Logf("memory-bootstrap complete (marker present); nothing to do")
		return nil
	}

	scanned := loadCheckpoint(ckptPath)
	candidates, err := fetchBootstrapCandidates(ctx, k)
	if err != nil {
		return fmt.Errorf("memory-bootstrap: list chats: %w", err)
	}

	var remaining []bootstrapChat
	for _, c := range candidates {
		if !scanned[c.JID] {
			remaining = append(remaining, c)
		}
	}
	k.Logf("memory-bootstrap: %d candidates, %d already scanned, %d remaining",
		len(candidates), len(candidates)-len(remaining), len(remaining))

	if len(remaining) == 0 {
		_ = os.WriteFile(donePath, []byte(time.Now().Format(time.RFC3339)+"\n"), 0644)
		_ = k.Notify("🧠 Memory bootstrap complete",
			fmt.Sprintf("Scanned %d WhatsApp chats and built the initial memory. Ongoing context is kept fresh by hot-sync.", len(candidates)))
		return nil
	}

	wacli := strings.TrimSpace(k.Config("wacli"))
	if wacli == "" {
		wacli = hostpaths.Wacli()
	}

	totalFacts, chatsDone := 0, 0
	var todos []string
	for b := 0; b < bootstrapMaxBatches && len(remaining) > 0; b++ {
		if ctx.Err() != nil {
			break
		}
		n := bootstrapBatchSize
		if n > len(remaining) {
			n = len(remaining)
		}
		batch := remaining[:n]
		remaining = remaining[n:]

		facts, batchTodos, err := scanChatBatch(ctx, k, wacli, batch)
		if err != nil {
			// Don't checkpoint a failed batch — it will retry next tick.
			k.Logf("memory-bootstrap: batch failed (will retry next tick): %v", err)
			break
		}
		for _, f := range facts {
			if err := k.Remember(f); err != nil {
				k.Logf("memory-bootstrap: remember failed: %v", err)
			}
		}
		todos = append(todos, batchTodos...)
		totalFacts += len(facts)
		chatsDone += len(batch)
		appendCheckpoint(ckptPath, batch)
		k.Logf("memory-bootstrap: batch %d done — %d chats, %d facts, %d todos (%d chats left)",
			b+1, len(batch), len(facts), len(batchTodos), len(remaining))
	}

	// Don't just remember — hand actionable items to the act-on-pending loop,
	// which completes what it safely can and flags real decisions. Discovery
	// (here) is decoupled from execution (that loop) via a persisted queue.
	if len(todos) > 0 {
		if err := enqueuePending(todos); err != nil {
			k.Logf("memory-bootstrap: enqueue pending failed: %v", err)
		} else if err := k.RunLoop("act-on-pending"); err != nil {
			k.Logf("memory-bootstrap: could not trigger act-on-pending (will run on its schedule): %v", err)
		}
	}

	if chatsDone > 0 {
		_ = k.Notify("🧠 Memory bootstrap progress",
			fmt.Sprintf("Learned %d facts from %d chats this pass (%d actionable items queued); %d chats remaining (continues automatically).",
				totalFacts, chatsDone, len(todos), len(remaining)))
	}
	return nil
}

// scanChatBatch delegates one batch of chats to the Claude harness, which reads
// each conversation via the wacli CLI and distills durable facts plus any
// still-actionable pending items (TODOs).
func scanChatBatch(ctx context.Context, k loopkit.Kit, wacli string, batch []bootstrapChat) (facts, todos []string, err error) {
	var list strings.Builder
	for _, c := range batch {
		kind := "direct chat"
		if c.IsGroup {
			kind = "group"
		}
		fmt.Fprintf(&list, "- %q | jid: %s | %s\n", c.Name, c.JID, kind)
	}

	prompt := "You are building your operator's long-term memory from their WhatsApp history, using the wacli CLI at " + wacli + ".\n\n" +
		"Chats in this batch:\n" + list.String() + "\n" +
		"For EACH chat:\n" +
		fmt.Sprintf("1. Run: %s messages --chat \"<jid>\" --limit %d   (messages come oldest-first; is_from_me=true means the operator wrote it)\n", wacli, bootstrapMsgsPerChat) +
		"2. If it's clearly a promotional/community/broadcast feed the operator doesn't really participate in, skip it entirely.\n" +
		"3. Otherwise distill DURABLE facts a personal assistant should remember: who the person/group is to the operator (relationship, role, company), ongoing projects and deals, commitments and deadlines (with concrete dates), decisions made, preferences, and important life/work facts. Each fact must be ONE standalone sentence with names/dates spelled out (it will be read with no other context). Never output raw chit-chat, greetings, or message dumps. At most 25 facts per chat; fewer is better than padding.\n" +
		"4. Additionally, if the chat shows something STILL PENDING that an assistant could complete or must surface — an unfulfilled promise by the operator, an unanswered direct request to them, an agreed meeting/deadline that may not be scheduled yet — output it as a TODO line with the chat name, jid, and concrete details/dates. Only genuinely open, recent items; not stale or already-resolved ones.\n\n" +
		"Output format, no other text:\n" +
		"FACT: <one standalone fact>\n" +
		"TODO: <chat name> | <jid> | <what is pending, with dates>"

	out, err := k.Harness(ctx, prompt)
	if err != nil {
		return nil, nil, err
	}
	if looksLikeError(out) {
		return nil, nil, fmt.Errorf("harness returned an error/refusal: %.120s", out)
	}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if f, ok := strings.CutPrefix(line, "FACT:"); ok {
			if f = strings.TrimSpace(f); len(f) > 10 {
				facts = append(facts, f)
			}
		} else if t, ok := strings.CutPrefix(line, "TODO:"); ok {
			if t = strings.TrimSpace(t); len(t) > 10 {
				todos = append(todos, t)
			}
		}
	}
	return facts, todos, nil
}

// fetchBootstrapCandidates lists chats worth scanning: unlocked, active within
// the window, not newsletters/broadcasts, and not the operator's own chats.
func fetchBootstrapCandidates(ctx context.Context, k loopkit.Kit) ([]bootstrapChat, error) {
	body, status, err := k.HTTP(ctx, "GET", hostpaths.WacliAPIURL()+"/chats?limit=1000", nil, "")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("wacli /chats: HTTP %d", status)
	}
	var resp struct {
		Chats []bootstrapChat `json:"chats"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("parse /chats: %w", err)
	}

	operator := operatorChatSet()
	cutoff := time.Now().AddDate(0, 0, -bootstrapActiveDays)

	var out []bootstrapChat
	for _, c := range resp.Chats {
		if c.Locked || c.JID == "" {
			continue
		}
		if strings.Contains(c.JID, "@newsletter") || strings.Contains(c.JID, "@broadcast") || strings.HasPrefix(c.JID, "status@") {
			continue
		}
		if operator[normalizeChatID(c.JID)] {
			continue
		}
		t, err := time.Parse(time.RFC3339, c.LastMessageAt)
		if err != nil || t.Before(cutoff) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// operatorChatSet returns the operator's own chat ids (normalized) from the
// environment, so the bootstrap doesn't scan KARMAX's own command chats.
func operatorChatSet() map[string]bool {
	set := make(map[string]bool)
	for _, env := range []string{os.Getenv("WHATSAPP_OPERATOR_CHATS"), os.Getenv("WHATSAPP_TARGET")} {
		for _, p := range strings.Split(env, ",") {
			if n := normalizeChatID(p); n != "" {
				set[n] = true
			}
		}
	}
	return set
}

func normalizeChatID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "@:"); i >= 0 {
		s = s[:i]
	}
	return s
}

func bootstrapPaths() (ckpt, done string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(home, ".karmax")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "memory-bootstrap.ckpt"), filepath.Join(dir, "memory-bootstrap.done"), nil
}

func loadCheckpoint(path string) map[string]bool {
	set := make(map[string]bool)
	data, err := os.ReadFile(path)
	if err != nil {
		return set
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			set[line] = true
		}
	}
	return set
}

func appendCheckpoint(path string, batch []bootstrapChat) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	for _, c := range batch {
		fmt.Fprintln(f, c.JID)
	}
}
