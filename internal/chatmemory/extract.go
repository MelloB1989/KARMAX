package chatmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
)

// recallK is how many existing memories one batch is set beside.
const recallK = 12

const instructions = `You keep %[1]s's long-term memory in step with %[1]s's WhatsApp. Below are new messages from one chat, and what memory already holds that may be related to them.

Decide what in these messages is worth knowing months from now, and what it changes in memory:
- add: a durable fact memory does not hold yet — a commitment, decision, deadline, plan, a change in a project's status, a fact about a person (role, company, relationship, where they are, what they are working on), or a preference %[1]s stated.
- update: a memory listed below that these messages show is now wrong or out of date. Give its number and the whole corrected fact.
- forget: a memory listed below that these messages show is no longer true and has nothing to replace it (a cancelled plan, a task now done). Give its number.

Rules:
- One fact per op, standalone months from now: full names as the chat shows them, absolute dates (2026-10-29, never "tomorrow"), and who said or asked it when that matters. A request from someone else is theirs ("Priya asked %[1]s to…"), never %[1]s's instruction.
- Lines from "%[1]s" are %[1]s's own words. "KARMAX (on %[1]s's account)" is %[1]s's assistant speaking on that account: what it says is not %[1]s's own statement.
- %[1]s has no other name. Who wrote each line is decided from the account, not from what anyone is called, so never store that %[1]s is also known as something, that %[1]s is someone else, or that someone else is %[1]s — not from a display name, a contact label, a login or an email address.
- Never store a password, OTP, PIN, API key, token, card number or any other secret, not even with a note that it was shared. "Priya shared the portal login" is fine; the login itself is not.
- Skip greetings, small talk, jokes, stickers, forwarded news and anything memory already holds.
- Refer to memories ONLY by the numbers listed. Most batches change nothing: then answer {"ops":[]}.

subject: who or what the fact is about, one or two words (a person's name, a company, a project).
category: relationship (about a person) | project | decision | preference (%[1]s's own) | reference | task (a dated commitment or to-do) | context
importance: low | medium | high

Answer with ONLY this JSON:
{"ops":[{"op":"add|update|forget","memory":<number, for update and forget>,"fact":"…","subject":"…","category":"…","importance":"…","when":"YYYY-MM-DD"}]}
`

// op is one change the model proposes.
type op struct {
	Op         string `json:"op"`
	Memory     int    `json:"memory"`
	Fact       string `json:"fact"`
	Subject    string `json:"subject"`
	Category   string `json:"category"`
	Importance string `json:"importance"`
	When       string `json:"when"`
}

// extract asks the model what one batch changes, and applies it.
func (s *Syncer) extract(ctx context.Context, chat Chat, lines []string, at time.Time, res *Result) error {
	shown := s.recall(chat, lines)

	var b strings.Builder
	fmt.Fprintf(&b, instructions, s.cfg.Operator)
	kind := "direct chat"
	if chat.IsGroup {
		kind = "group"
	}
	name := chat.Name
	if name == "" {
		name = "+" + bare(chat.ID)
	}
	fmt.Fprintf(&b, "\nCHAT: %s (%s)\n\nMEMORY:\n", name, kind)
	if len(shown) == 0 {
		b.WriteString("(nothing related)\n")
	}
	for i, r := range shown {
		fmt.Fprintf(&b, "[%d] %s\n", i+1, oneLine(r.Entry.Content, 300))
	}
	b.WriteString("\nMESSAGES:\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}

	reply, err := s.ask(ctx, b.String())
	if err != nil {
		return fmt.Errorf("wa-memory: extract %s: %w", chat.ID, err)
	}
	ops, err := parseOps(reply)
	if err != nil {
		// A reply that is not the JSON asked for changes nothing, and asking
		// again would most likely get the same reply.
		s.log.Warn("wa-memory: unreadable extraction; batch skipped", zapChat(chat), zapErr(err))
		return nil
	}
	s.apply(ctx, ops, shown, at, res)
	return nil
}

// recall is what memory holds that a batch may bear on: its chat's name and
// what was said, one handle per memory.
func (s *Syncer) recall(chat Chat, lines []string) []memory.SearchResult {
	q := chat.Name + " " + strings.Join(lines, " ")
	if len(q) > 1500 {
		q = q[:1500]
	}
	hits, err := s.mem.SearchWith(q, recallK, memory.SearchOpts{})
	if err != nil {
		s.log.Warn("wa-memory: recall failed; extracting without it", zapChat(chat), zapErr(err))
		return nil
	}
	seen := map[string]bool{}
	out := hits[:0:0]
	for _, h := range hits {
		if h.Entry.ID == "" || seen[h.Entry.ID] || strings.TrimSpace(h.Entry.Content) == "" {
			continue
		}
		seen[h.Entry.ID] = true
		out = append(out, h)
	}
	return out
}

// parseOps reads the model's JSON, tolerating a code fence or prose around it.
func parseOps(reply string) ([]op, error) {
	i, j := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if i < 0 || j <= i {
		return nil, fmt.Errorf("no JSON object in %q", oneLine(reply, 120))
	}
	var out struct {
		Ops []op `json:"ops"`
	}
	if err := json.Unmarshal([]byte(reply[i:j+1]), &out); err != nil {
		return nil, err
	}
	return out.Ops, nil
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
