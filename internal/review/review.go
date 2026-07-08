// Package review runs KARMAX's staleness check-ins: it finds memory,
// reminders, and commitments that are OLD and time-sensitive, asks the operator
// (once) whether each is still relevant, and delivers the question to the app
// AND WhatsApp. The operator can answer from either channel; answering closes
// the review everywhere (see the agent's review context + review.resolve tool
// and the /api/reviews endpoints).
//
// Design constraints (learned from the reminder-spam incident):
//   - LATCHED: every item is asked about at most once (unique dedup key); a
//     resolved or dismissed item is never re-surfaced.
//   - CAPPED: no more than maxOpenReviews are open at a time, so "aggressive"
//     detection never becomes a flood.
//   - ONE PER TICK: each pass raises at most one new question.
package review

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// maxOpenReviews caps concurrent unanswered check-ins. Aggressive cadence means
// we notice staleness fast — not that we pile on. The operator answering frees
// slots for the next batch.
const maxOpenReviews = 8

// staleAfter is the age past which a time-sensitive fact is a candidate for a
// "still relevant?" check-in.
const staleAfter = 4 * 24 * time.Hour

// Config carries what the reviewer needs; the runtime builds it from the agent.
type Config struct {
	Namespace string
	AgentID   string
	Provider  string
	Model     string
	Fallbacks []karmahelper.FallbackModel
	// Deliver the check-in to WhatsApp. WAChannelID/WATarget come from the
	// comms channel; SendFunc is the comms manager's Send. Both optional.
	WAChannelID string
	WATarget    string
	SendFunc    func(channelID, target, content string) error
}

type Reviewer struct {
	cfg   Config
	store *store.Store
	log   *zap.Logger
}

func New(cfg Config, s *store.Store, log *zap.Logger) *Reviewer {
	return &Reviewer{cfg: cfg, store: s, log: log}
}

// judgePrompt asks the cheap model to pick the single stalest, most-worth-asking
// candidate and phrase a tight yes/no-ish question with quick options.
const judgePrompt = `You maintain the operator's long-term memory by catching things that have gone STALE. Below are memory entries and reminders, each tagged with how long ago it was stored and its content.

Pick the SINGLE item most worth a quick "is this still relevant?" check — strongly prefer items that are TIME-SENSITIVE and now OLD: a past deadline, an unfulfilled commitment ("X will get back by Friday"), a "temporary" plan, an open task, a promised action, an event whose date has passed. Ignore durable identity facts (who someone is, stable preferences) — those don't go stale. If NOTHING is genuinely stale and worth asking, return empty.

Write the question in the operator's second person, short and concrete, referencing the item and its age. Give 2-3 one-tap options that let them resolve it, e.g. ["Done","Still open","Drop it"] or ["Yes still on","Cancelled","Reschedule"].

Respond with ONLY JSON, no prose:
{"idx": <0-based index of the chosen item, or -1 if none>, "question": "<question>", "options": ["...","..."], "resolution_hint": "<one line: what to do with the memory if they say it's done/dropped>"}`

// Tick runs one staleness pass: gather candidates, ask the model to pick one,
// create the review (latched + capped), and deliver it. At most one new
// question per call.
func (r *Reviewer) Tick(ctx context.Context) error {
	ns := r.cfg.Namespace

	open, err := r.store.CountOpenReviews(ns)
	if err != nil {
		return fmt.Errorf("count open reviews: %w", err)
	}
	if open >= maxOpenReviews {
		r.log.Debug("review: at open-question cap; skipping", zap.Int("open", open))
		return nil
	}

	candidates := r.gatherCandidates(ns)
	if len(candidates) == 0 {
		return nil
	}

	// Ask the model to pick the stalest worth-asking item.
	var list strings.Builder
	for i, c := range candidates {
		fmt.Fprintf(&list, "%d. [%s | stored %s] %s\n", i, c.kind, humanAge(c.at), oneLine(c.text, 240))
	}
	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Provider: r.cfg.Provider, Model: r.cfg.Model, MaxTokens: 400,
		SystemPrompt: judgePrompt, FallbackModels: r.cfg.Fallbacks,
	}, nil)
	resp, _, _, err := sess.Chat(ctx, "Candidates (newest-relevant first):\n\n"+list.String())
	if err != nil {
		return fmt.Errorf("review judge: %w", err)
	}
	var pick struct {
		Idx            int      `json:"idx"`
		Question       string   `json:"question"`
		Options        []string `json:"options"`
		ResolutionHint string   `json:"resolution_hint"`
	}
	if err := json.Unmarshal([]byte(extractJSON(resp)), &pick); err != nil {
		r.log.Debug("review: unparseable judge response", zap.String("resp", oneLine(resp, 200)))
		return nil
	}
	if pick.Idx < 0 || pick.Idx >= len(candidates) || strings.TrimSpace(pick.Question) == "" {
		return nil // nothing stale enough
	}

	c := candidates[pick.Idx]
	dedup := c.dedupKey()
	if has, _ := r.store.HasReview(ns, dedup); has {
		return nil // already asked once — never re-ask
	}

	opts, _ := json.Marshal(pick.Options)
	rev := store.StoredReview{
		ID: uuid.New().String(), Namespace: ns,
		TargetKind: c.kind, TargetID: c.id, DedupKey: dedup,
		Question: strings.TrimSpace(pick.Question), Options: string(opts),
		Context: pick.ResolutionHint,
	}
	if err := r.store.CreateReview(rev); err != nil {
		return fmt.Errorf("create review: %w", err)
	}
	r.log.Info("review: raised staleness check-in", zap.String("kind", c.kind), zap.String("q", oneLine(pick.Question, 100)))
	r.deliver(rev)
	return nil
}

// deliver sends the check-in to the app feed (+push) and WhatsApp. The operator
// can answer in either place.
func (r *Reviewer) deliver(rev store.StoredReview) {
	var opts []string
	_ = json.Unmarshal([]byte(rev.Options), &opts)
	body := rev.Question
	if len(opts) > 0 {
		body += "\n\nReply: " + strings.Join(opts, " / ")
	}
	// App feed + push.
	builtin.PushAppNotification(r.store, r.cfg.AgentID, "review", "🕰️ Still relevant?", body)
	// WhatsApp.
	if r.cfg.SendFunc != nil && r.cfg.WAChannelID != "" && r.cfg.WATarget != "" {
		if err := r.cfg.SendFunc(r.cfg.WAChannelID, r.cfg.WATarget, "🕰️ "+body); err != nil {
			r.log.Warn("review: whatsapp deliver failed", zap.Error(err))
		}
	}
}

// ---- candidate gathering ---------------------------------------------------

type candidate struct {
	kind string // memory | reminder
	id   string
	text string
	at   time.Time
}

func (c candidate) dedupKey() string {
	h := sha1.Sum([]byte(c.kind + "|" + c.id + "|" + c.text))
	return fmt.Sprintf("%x", h[:10])
}

// gatherCandidates collects stale, time-sensitive items: old non-pinned memory
// entries, and failed/old reminders that never landed.
func (r *Reviewer) gatherCandidates(ns string) []candidate {
	cutoff := time.Now().Add(-staleAfter)
	var out []candidate

	// Old, non-pinned memory entries (the model filters to time-sensitive ones).
	if entries, err := r.store.ListMemoryEntries(ns, 400); err == nil {
		for _, e := range entries {
			if e.Pinned || e.CreatedAt.After(cutoff) {
				continue
			}
			// Cheap pre-filter: only consider entries whose text hints at time.
			if !looksTimeSensitive(e.Content) {
				continue
			}
			out = append(out, candidate{kind: "memory", id: e.ID, text: e.Content, at: e.CreatedAt})
			if len(out) >= 40 {
				break
			}
		}
	}

	// Reminders that failed or are old and unresolved.
	if acts, err := r.store.ListDeviceActions("", 100); err == nil {
		for _, a := range acts {
			if a.Kind != "reminder" || a.CreatedAt.After(cutoff) {
				continue
			}
			out = append(out, candidate{kind: "reminder", id: a.ID, text: oneLine(a.Payload, 200), at: a.CreatedAt})
		}
	}

	// Oldest first (the stalest are the point), and cap the list so the judge
	// prompt stays small and fast — a big prompt through the gateway times out.
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// looksTimeSensitive is a cheap keyword pre-filter so the model only judges
// entries that plausibly have a temporal component.
func looksTimeSensitive(s string) bool {
	l := strings.ToLower(s)
	for _, kw := range []string{
		"deadline", "by ", "due", "tomorrow", "today", "this week", "next week",
		"friday", "monday", "tuesday", "wednesday", "thursday", "saturday", "sunday",
		"get back", "will ", "plan to", "promised", "commit", "meeting", "call ",
		"send ", "follow up", "follow-up", "pending", "waiting", "asap", "soon",
		"schedule", "reschedule", "temporary", "for now", "later", "pay", "invoice",
	} {
		if strings.Contains(l, kw) {
			return true
		}
	}
	return false
}

// ---- small helpers ---------------------------------------------------------

func humanAge(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%d weeks ago", int(d.Hours()/(24*7)))
	default:
		return fmt.Sprintf("%d months ago", int(d.Hours()/(24*30)))
	}
}

func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// extractJSON pulls the first {...} object out of a model response that may be
// fenced or prose-wrapped.
func extractJSON(s string) string {
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}
