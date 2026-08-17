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
	"unicode"

	"github.com/MelloB1989/karmax/internal/memory"
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
	// mem is the memory store. Reviews are about memories going stale, so this
	// has to be whatever is actually holding them — reading the local table
	// once GitLoom owned memory meant reviewing a frozen snapshot and asking
	// the operator about facts from before the cutover, forever.
	mem *memory.Manager
	log *zap.Logger
}

func New(cfg Config, s *store.Store, mem *memory.Manager, log *zap.Logger) *Reviewer {
	return &Reviewer{cfg: cfg, store: s, mem: mem, log: log}
}

// judgePrompt asks the cheap model to pick the single stalest, most-worth-asking
// candidate and phrase a tight yes/no-ish question with quick options.
const judgePrompt = `You maintain the operator's long-term memory by catching things that have gone STALE. Below are memory entries and reminders, each tagged with how long ago it was stored and its content.

Pick the SINGLE item most worth a quick "is this still relevant?" check — strongly prefer items that are TIME-SENSITIVE and now OLD: a past deadline, an unfulfilled commitment ("X will get back by Friday"), a "temporary" plan, an open task, a promised action, an event whose date has passed. Ignore durable identity facts (who someone is, stable preferences) — those don't go stale. If NOTHING is genuinely stale and worth asking, return empty.

Every date word inside an entry — "today", "tomorrow", "this week", "Friday" — was written WHEN THE ENTRY WAS STORED, not now. Work out the real date from the entry's age before you use it, and never repeat a relative word from the entry as if it still holds. Asking "did you make the meeting scheduled for today (Aug 11)?" four days after Aug 11 tells the operator you are not reading the calendar.

Before choosing an item, check whether ANOTHER entry in the list already answers it — a later entry saying the thing was done, dropped, delivered or rescheduled. If one does, that item is resolved: do not ask about it, pick something else or return -1.

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

	candidates := r.gatherCandidates(ctx, ns)
	if len(candidates) == 0 {
		return nil
	}

	// Ask the model to pick the stalest worth-asking item.
	var list strings.Builder
	for i, c := range candidates {
		fmt.Fprintf(&list, "%d. [%s | stored %s] %s\n", i, c.kind, humanAge(c.at), oneLine(c.text, 240))
	}
	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Kind:     "review",
		Provider: r.cfg.Provider, Model: r.cfg.Model, MaxTokens: 400,
		SystemPrompt: judgePrompt, FallbackModels: r.cfg.Fallbacks,
	}, nil)
	// Today's date, because the model had none. Entries say "today" and
	// "tomorrow" meaning the day they were written, and without knowing the
	// current date the model repeated those words verbatim days later.
	resp, _, _, err := sess.Chat(ctx, fmt.Sprintf("Today is %s.\n\nCandidates (newest-relevant first):\n\n%s",
		time.Now().Format("Monday, 2 January 2006"), list.String()))
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
	// And not the same QUESTION in different words.
	//
	// The key above identifies a row. The operator experiences a subject: one
	// commitment to Shravan, four memory entries about it — two of them
	// identical, one written by the merge pass — so four different keys, and
	// they were asked three times in three wordings across two days, about
	// something a fourth entry already recorded as dropped. Comparing the
	// questions catches what comparing the rows cannot.
	if earlier, dup := r.askedRecently(ns, pick.Question); dup {
		r.log.Info("review: not asking again in different words",
			zap.String("question", oneLine(pick.Question, 90)),
			zap.String("already_asked", oneLine(earlier, 90)))
		return nil
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
func (r *Reviewer) gatherCandidates(ctx context.Context, ns string) []candidate {
	cutoff := time.Now().Add(-staleAfter)
	var out []candidate

	// Old, non-pinned memories (the model then filters to time-sensitive ones).
	//
	// Two steps, and the order is the point. The survey is ONE call and carries
	// summaries but no dates; the text pre-filter runs on those, and only the
	// handful that survive are fetched for their age. Asking the store for four
	// hundred memories in full to discard all but a dozen would be a request per
	// memory to answer a question a substring match already answered.
	out = append(out, r.staleMemories(ctx, cutoff)...)

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

// staleMemories returns old memories whose text hints at something temporal.
//
// The pre-filter runs on summaries, before any per-memory request, so a store
// full of durable identity facts costs one call rather than hundreds.
func (r *Reviewer) staleMemories(ctx context.Context, cutoff time.Time) []candidate {
	if r.mem == nil {
		return nil
	}
	surveyed, err := r.mem.Survey(ctx, 400)
	if err != nil {
		r.log.Warn("review: could not survey memory", zap.Error(err))
		return nil
	}

	var out []candidate
	for _, e := range surveyed {
		if e.Pinned || !looksTimeSensitive(e.Content) {
			continue
		}
		// The date is why this one is fetched: staleness is the whole judgement
		// and the survey does not carry it.
		full, err := r.mem.Load(ctx, e.ID)
		if err != nil || full == nil {
			continue
		}
		if full.CreatedAt.IsZero() || full.CreatedAt.After(cutoff) {
			continue
		}
		text := full.Content
		if strings.TrimSpace(text) == "" {
			text = e.Content
		}
		out = append(out, candidate{kind: "memory", id: full.ID, text: oneLine(text, 400), at: full.CreatedAt})
		if len(out) >= 40 {
			break
		}
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

// askedSimilarWithin is how far back to look for the same question in
// different words. Long enough to cover a memory pass rewriting an entry and
// the review loop coming round again; short enough that a genuinely recurring
// commitment can be raised afresh next month.
const askedSimilarWithin = 21 * 24 * time.Hour

// Two questions are the same question if they overlap heavily, OR if they
// share at least two distinctive words.
//
// The ratio alone is not enough, and the real repeats show why: "Shravan Kumar
// podcast scripting — you promised this on Jul 12" and "Shravan's
// scripting/planning work — you committed to this on Jul 12" share exactly
// "shravan" and "scripting" out of seven and nine significant words. Two out
// of seven is 0.29, which no honest ratio threshold would catch without also
// catching unrelated pairs. But WHICH two matters: a shared proper noun and a
// shared project name is the subject itself, while sharing "work" and "days"
// is nothing. Long words carry subjects, so two shared long words is the
// signal, and one is not — "did you send Shiva the APK" and "did you set up
// Shiva's PC" share only the name and remain two different questions.
//
// Both rules are heuristics tuned on the messages the operator actually
// received, and they are deliberately conservative: a missed duplicate is a
// second nag, while a false match silently swallows a question that was worth
// asking.
const (
	similarEnough     = 0.5
	minSharedForRatio = 3
	distinctiveLen    = 5
	distinctiveNeeds  = 2
)

// askedRecently reports whether a question of the same substance has already
// gone out, and which one.
func (r *Reviewer) askedRecently(ns, question string) (string, bool) {
	asked, err := r.store.RecentReviewQuestions(ns, time.Now().Add(-askedSimilarWithin), 60)
	if err != nil || len(asked) == 0 {
		return "", false
	}
	want := significantWords(question)
	if len(want) < 3 {
		// Too short to judge by overlap; the exact latch is all there is.
		return "", false
	}
	for _, prev := range asked {
		if sameQuestion(question, prev) {
			return prev, true
		}
	}
	return "", false
}

// sameQuestion reports whether two review questions are asking the same thing.
func sameQuestion(a, b string) bool {
	want, have := significantWords(a), significantWords(b)
	if len(want) < 3 || len(have) == 0 {
		return false
	}
	{
		shared := 0
		for w := range want {
			if have[w] {
				shared++
			}
		}
		// Against the SMALLER set: one question phrased at length and the same
		// question phrased briefly are still the same question, and dividing by
		// the union would let verbosity hide a repeat.
		smaller := len(want)
		if len(have) < smaller {
			smaller = len(have)
		}
		// A ratio needs enough words to mean anything. "Did you send Shiva the
		// APK" and "Did you set up Shiva's laptop" share one word out of two
		// and score a perfect 0.5 — two different questions about one person,
		// which is exactly what must not be swallowed.
		if shared >= minSharedForRatio && smaller > 0 && float64(shared)/float64(smaller) >= similarEnough {
			return true
		}
		distinctive := 0
		for w := range want {
			if have[w] && len([]rune(w)) >= distinctiveLen {
				distinctive++
			}
		}
		if distinctive >= distinctiveNeeds {
			return true
		}
	}
	return false
}

// significantWords reduces a question to the words that carry its subject —
// names, projects, verbs — dropping the scaffolding every review question
// shares ("still", "should", "reply", "done") which would otherwise make any
// two of them look alike.
func significantWords(s string) map[string]bool {
	out := map[string]bool{}
	// Split on anything that is not a letter, rather than on spaces. The three
	// repeated questions wrote the same subject as "Shravan", "Shravan's" and
	// "scripting/planning": trimming edge punctuation leaves those as three
	// different tokens and the overlap comes out at zero, which is how the
	// first cut of this check would have let all three through again.
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r)
	}) {
		if len(w) < 4 || reviewStopWords[w] {
			continue
		}
		out[w] = true
	}
	return out
}

var reviewStopWords = map[string]bool{
	"still": true, "should": true, "reply": true, "done": true, "drop": true,
	"this": true, "that": true, "with": true, "from": true, "have": true,
	"your": true, "you": true, "the": true, "and": true, "for": true,
	"was": true, "did": true, "does": true, "is": true, "it": true,
	"pending": true, "happening": true, "needed": true, "resolved": true,
	"progress": true, "planning": true, "there": true, "about": true,
	"or": true, "on": true, "in": true, "to": true, "a": true,
}
