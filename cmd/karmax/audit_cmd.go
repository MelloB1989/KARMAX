package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	gitloom "github.com/GitLoomHQ/gitloom-go/gitloom"
	"github.com/spf13/cobra"
)

// Auditing memory against what actually happened.
//
// A memory store that only ever appends becomes wrong in a particular way: the
// facts were true when written and nobody ever went back. "Rishwak is
// escalating about the late APK" was accurate on 23 July and is misleading in
// August, but nothing in the system notices, so the agent keeps briefing the
// operator on a resolved complaint.
//
// The fix is an adversarial pair. One model reads a memory and asks what would
// have to be true for it to still hold — as questions that can be checked
// against evidence, not as opinions. Another answers ONLY from WhatsApp
// history, which is the record of what really happened. A third call weighs the
// two. Splitting them matters: a single model asked "is this still true?" will
// reason from the memory itself and confirm whatever it was given.
//
// Updates are applied, not queued. Every write is a git commit in GitLoom, so
// the audit trail is `git log` and the undo is `git revert` — which is a better
// safety property than an approval queue nobody drains.

const auditModel = "sonnet"

func auditCmd() *cobra.Command {
	var (
		namespace   string
		topics      []string
		limit       int
		concurrency int
		dryRun      bool
		staleDays   int
		verbose     bool
	)
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Re-check stored memories against WhatsApp history and update the stale ones",
		Long: "Two models working against each other: one asks what would falsify a\n" +
			"memory, the other answers only from wacli chat history. Stale memories\n" +
			"are rewritten in GitLoom as git commits, so `git log` is the audit\n" +
			"trail and `git revert` is the undo.",
		RunE: func(c *cobra.Command, _ []string) error {
			return runAudit(c.Context(), auditOptions{
				Namespace: namespace, Topics: topics, Limit: limit,
				Concurrency: concurrency, DryRun: dryRun,
				StaleDays: staleDays, Verbose: verbose,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&namespace, "namespace", "", "GitLoom namespace")
	f.StringSliceVar(&topics, "topic", []string{"facts/people", "facts/projects"},
		"topic prefixes to audit")
	f.IntVar(&limit, "limit", 15, "how many memories to audit (0 = all)")
	f.IntVar(&concurrency, "concurrency", 3, "memories audited in parallel")
	f.BoolVar(&dryRun, "dry-run", false, "show verdicts without writing anything")
	f.IntVar(&staleDays, "stale-days", 14, "only audit memories not updated in this many days")
	f.BoolVar(&verbose, "verbose", false, "print the questions, evidence and reasoning")
	return cmd
}

type auditOptions struct {
	Namespace   string
	Topics      []string
	Limit       int
	Concurrency int
	DryRun      bool
	StaleDays   int
	Verbose     bool
}

// --- the three model calls -------------------------------------------------

// auditPlan is what the first model produces: what to check, and where.
type auditPlan struct {
	Subject string `json:"subject"`
	Checks  []struct {
		Claim string `json:"claim"`
		// Query is what to search WhatsApp for. Short keyword phrases beat
		// sentences: this is a substring search over messages, not a model.
		Query string `json:"query"`
		// ChatRef narrows to one conversation when the memory names a person.
		ChatRef string `json:"chat_ref"`
	} `json:"checks"`
}

// verdict is what the third model produces after seeing the evidence.
type verdict struct {
	Status string `json:"status"` // current | stale | unverifiable
	// Reason is one sentence, and must cite the evidence rather than restate
	// the memory — a "reason" that only paraphrases the input is the failure
	// mode this whole design exists to avoid.
	Reason string `json:"reason"`
	// Replacement is the corrected memory body. Only read when stale.
	Replacement string  `json:"replacement"`
	Confidence  float64 `json:"confidence"`
}

// ask runs one prompt through the Claude Code harness.
//
// The harness rather than the model gateway because the gateway is a single
// process that has been down for three days at a time; this path has no such
// dependency. Sonnet because the job is bulk judgement over hundreds of short
// documents, where throughput matters more than depth.
func ask(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", auditModel, prompt)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("claude: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("claude: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// decodeJSON pulls a JSON object out of a model reply. Models wrap JSON in a
// markdown fence even when told not to, and failing the memory over backticks
// would discard everything the model got right.
func decodeJSON(reply string, into any) error {
	s := strings.TrimSpace(reply)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		if j := strings.IndexByte(s, '\n'); j >= 0 {
			s = s[j+1:]
		}
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	start := strings.IndexAny(s, "{")
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return fmt.Errorf("no JSON object in reply: %s", truncQ(reply, 200))
	}
	return json.Unmarshal([]byte(s[start:end+1]), into)
}

func planFor(ctx context.Context, path, content string) (*auditPlan, error) {
	prompt := fmt.Sprintf(`You are auditing one memory from a personal assistant's long-term store.
Your job is NOT to decide whether it is true. Your job is to say what evidence would
settle it, so a second agent can go and look.

MEMORY PATH: %s
MEMORY:
%s

Produce checks for the claims that could plausibly have CHANGED since they were
written — a delivery that may have landed, a deadline that may have passed, a
dispute that may have resolved, a role someone may have left. Ignore claims that
cannot go stale (where someone was born, what a project is named).

For each check write a WhatsApp search query: 2-4 keywords that would appear in
messages about it. Not a sentence — this is substring search over message text.
If the memory names a person, put their name or number in chat_ref; otherwise "".

Reply with ONLY this JSON, at most 3 checks:
{"subject":"<who or what this memory is about>",
 "checks":[{"claim":"<the specific claim>","query":"<keywords>","chat_ref":"<person or empty>"}]}`,
		path, truncQ(content, 3000))

	reply, err := ask(ctx, prompt)
	if err != nil {
		return nil, err
	}
	var p auditPlan
	if err := decodeJSON(reply, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func judge(ctx context.Context, path, content, evidence string) (*verdict, error) {
	prompt := fmt.Sprintf(`You are deciding whether a stored memory is still accurate, using ONLY the
WhatsApp evidence below. Today is %s.

MEMORY PATH: %s
STORED MEMORY:
%s

EVIDENCE FROM WHATSAPP HISTORY:
%s

Rules:
- "stale" ONLY if the evidence positively contradicts or resolves something the
  memory asserts. Absence of evidence is NOT staleness — say "unverifiable".
- Do not mark something stale because it merely sounds old.
- If stale, write the corrected memory in "replacement": keep everything still
  true, correct what changed, and state what changed with its date. Keep the
  same markdown structure, including any ## sections.
- "reason" must cite what the evidence SHOWED. A reason that only restates the
  memory means you did not use the evidence, and will be rejected.

Reply with ONLY this JSON:
{"status":"current|stale|unverifiable","reason":"<one sentence citing evidence>",
 "replacement":"<full corrected memory, or empty>","confidence":0.0-1.0}`,
		time.Now().Format("2006-01-02"), path, truncQ(content, 4000), truncQ(evidence, 6000))

	reply, err := ask(ctx, prompt)
	if err != nil {
		return nil, err
	}
	var v verdict
	if err := decodeJSON(reply, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// --- evidence gathering ----------------------------------------------------

// waMessage mirrors wacli's /messages response. The field names are load-
// bearing: an earlier version guessed `text`/`from_me`/`chat_name` and every
// message decoded into an empty struct, so the audit gathered hundreds of
// blank lines and called the whole corpus unverifiable.
type waMessage struct {
	Timestamp time.Time `json:"timestamp"`
	ChatJID   string    `json:"chat_jid"`
	SenderJID string    `json:"sender_jid"`
	FromMe    bool      `json:"is_from_me"`
	Content   string    `json:"content"`
}

// gather runs the plan's searches against wacli and renders what it found.
//
// Scoped to messages AFTER the memory was written: the question is what
// happened since, and returning the conversation the memory was extracted from
// would let the judge "confirm" the memory with its own source.
func gather(ctx context.Context, base string, plan *auditPlan, since time.Time, verbose bool) string {
	var b strings.Builder
	seen := map[string]bool{}
	found := 0

	for _, c := range plan.Checks {
		// wacli's search is a SUBSTRING match over message text, so a
		// multi-word query matches only if those exact words appear adjacent —
		// "portfolio sent Neelima" finds nothing while "portfolio" finds 20 and
		// "Neelima" finds 15. Every term is searched separately and the results
		// unioned, which is the difference between an audit that reads the
		// archive and one that reports "unverifiable" for everything.
		var msgs []waMessage
		for _, term := range searchTerms(c.Query) {
			q := url.Values{
				"query": {term},
				"limit": {"8"},
				"after": {since.Format(time.RFC3339)},
			}
			if strings.TrimSpace(c.ChatRef) != "" {
				q.Set("chat_ref", c.ChatRef)
			}
			got := fetchMessages(ctx, base, q)
			// A search scoped to one chat that finds nothing is usually the
			// chat_ref failing to resolve rather than the claim being false,
			// so retry unscoped before concluding silence.
			if len(got) == 0 && q.Has("chat_ref") {
				q.Del("chat_ref")
				got = fetchMessages(ctx, base, q)
			}
			msgs = append(msgs, got...)
		}

		fmt.Fprintf(&b, "\n### check: %s\n(searched %v since %s)\n",
			c.Claim, searchTerms(c.Query), since.Format("2006-01-02"))
		if len(msgs) == 0 {
			b.WriteString("no messages found\n")
			continue
		}
		for _, m := range msgs {
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			key := m.Timestamp.Format(time.RFC3339) + m.Content
			if seen[key] {
				continue
			}
			seen[key] = true
			who := m.SenderJID
			if m.FromMe {
				who = "operator"
			}
			if who == "" {
				who = m.ChatJID
			}
			fmt.Fprintf(&b, "- [%s] %s: %s\n",
				m.Timestamp.Format("2006-01-02"), shortJID(who), truncQ(oneLineStr(m.Content), 240))
			found++
		}
	}
	if found == 0 {
		return ""
	}
	return b.String()
}

func oneLineStr(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", ""))
}

// --- the job ---------------------------------------------------------------

type auditResult struct {
	Path    string
	Status  string
	Reason  string
	Applied bool
	Err     error
}

func runAudit(ctx context.Context, o auditOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	_, client, localNS, err := migrationDeps(o.Namespace, true)
	if err != nil {
		return err
	}
	ns := o.Namespace
	if ns == "" {
		ns = strings.TrimSpace(os.Getenv("GITLOOM_NAMESPACE"))
	}
	if ns == "" {
		ns = localNS
	}
	wacliBase := strings.TrimSpace(os.Getenv("KARMAX_WACLI_API_URL"))
	if wacliBase == "" {
		wacliBase = "http://127.0.0.1:8765"
	}
	if _, err := http.Get(wacliBase + "/status"); err != nil {
		return fmt.Errorf("wacli is not reachable at %s: %w", wacliBase, err)
	}

	// Enumerate candidates from the tree rather than from retrieval: an audit
	// has to see every memory, including the ones no query would surface.
	var candidates []string
	for _, topic := range o.Topics {
		tr, err := client.Tree(ctx, &gitloom.TreeOptions{Namespace: ns, Path: topic, Depth: 2})
		if err != nil {
			fmt.Printf("could not read %s: %v\n", topic, err)
			continue
		}
		collectFiles(tr.Tree, &candidates)
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return fmt.Errorf("no memories found under %v in namespace %q", o.Topics, ns)
	}

	cutoff := time.Now().AddDate(0, 0, -o.StaleDays)
	fmt.Printf("auditing memories under %v in namespace %q\n", o.Topics, ns)
	fmt.Printf("%d candidates; only those not updated since %s\n\n",
		len(candidates), cutoff.Format("2006-01-02"))

	type job struct {
		path    string
		content string
		updated time.Time
	}
	var jobs []job
	for _, p := range candidates {
		m, err := client.Get(ctx, p, &gitloom.RecallOptions{Namespace: ns})
		if err != nil || m == nil || strings.TrimSpace(m.Content) == "" {
			continue
		}
		// Staleness is measured by what the memory is ABOUT, not by when the
		// file was last touched. `updated` is a storage timestamp — the
		// migration set it to today for all 197 files, which would make the
		// whole corpus look fresh and audit nothing. `created` carries the
		// date of the newest fact in the file, which is the real "as of".
		asOf := parseWhen(m.Created)
		if asOf.IsZero() {
			asOf = parseWhen(m.Updated)
		}
		if !asOf.IsZero() && asOf.After(cutoff) {
			continue // too recent for anything to have gone stale
		}
		jobs = append(jobs, job{path: p, content: m.Content, updated: asOf})
	}
	// Oldest first: the most likely to be wrong, so a --limit spends its budget
	// where the return is highest.
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].updated.Before(jobs[j].updated) })
	if o.Limit > 0 && len(jobs) > o.Limit {
		jobs = jobs[:o.Limit]
	}
	if len(jobs) == 0 {
		fmt.Println("nothing is stale enough to audit")
		return nil
	}
	fmt.Printf("auditing %d memories with %d in parallel\n\n", len(jobs), o.Concurrency)

	var (
		mu      sync.Mutex
		results []auditResult
		sem     = make(chan struct{}, max1(o.Concurrency))
		wg      sync.WaitGroup
	)
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			r := auditResult{Path: j.path}
			defer func() {
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
				printAudit(r, o.Verbose)
			}()

			plan, err := planFor(ctx, j.path, j.content)
			if err != nil {
				r.Err = fmt.Errorf("plan: %w", err)
				return
			}
			if len(plan.Checks) == 0 {
				r.Status, r.Reason = "current", "nothing in it can go stale"
				return
			}
			if o.Verbose {
				for _, c := range plan.Checks {
					fmt.Printf("  ? %s  [%s]\n", c.Claim, c.Query)
				}
			}

			since := j.updated
			if since.IsZero() {
				since = time.Now().AddDate(0, -3, 0)
			}
			evidence := gather(ctx, wacliBase, plan, since, o.Verbose)
			if strings.TrimSpace(evidence) == "" {
				r.Status, r.Reason = "unverifiable", "no messages since it was written"
				return
			}

			v, err := judge(ctx, j.path, j.content, evidence)
			if err != nil {
				r.Err = fmt.Errorf("judge: %w", err)
				return
			}
			r.Status, r.Reason = v.Status, v.Reason

			if v.Status != "stale" || strings.TrimSpace(v.Replacement) == "" {
				return
			}
			// A "correction" that throws most of the memory away is far more
			// likely to be the model summarising than a real update, and it
			// would silently destroy history. Refuse rather than apply.
			if len(v.Replacement) < len(j.content)/3 {
				r.Status = "rejected"
				r.Reason = fmt.Sprintf("replacement was %d%% of the original — looks like a summary, not a correction",
					100*len(v.Replacement)/max1(len(j.content)))
				return
			}
			if o.Verbose {
				fmt.Printf("\n  ── proposed rewrite of %s ──\n", j.path)
				fmt.Printf("  BEFORE (%d bytes):\n%s\n", len(j.content), indent(truncQ(j.content, 700)))
				fmt.Printf("  AFTER  (%d bytes):\n%s\n\n", len(v.Replacement), indent(truncQ(v.Replacement, 700)))
			}
			if o.DryRun {
				return
			}
			if err := client.Write(ctx, []gitloom.Memory{{
				Path:    j.path,
				Content: v.Replacement,
				Date:    time.Now().Format("2006-01-02"),
			}}, &gitloom.WriteOptions{Namespace: ns}); err != nil {
				r.Err = fmt.Errorf("write: %w", err)
				return
			}
			r.Applied = true
		}(j)
	}
	wg.Wait()

	counts := map[string]int{}
	applied, failed := 0, 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			continue
		}
		counts[r.Status]++
		if r.Applied {
			applied++
		}
	}
	fmt.Printf("\n%s\n", strings.Repeat("─", 72))
	fmt.Printf("audited %d   current %d   stale %d   unverifiable %d   rejected %d   errors %d\n",
		len(results), counts["current"], counts["stale"], counts["unverifiable"], counts["rejected"], failed)
	if o.DryRun {
		fmt.Println("dry run: nothing was written")
	} else {
		fmt.Printf("%d memories updated in GitLoom (each is a git commit; revert to undo)\n", applied)
	}
	return nil
}

func printAudit(r auditResult, verbose bool) {
	switch {
	case r.Err != nil:
		fmt.Printf("  ✗ %-44s %v\n", shortPath(r.Path), r.Err)
	case r.Applied:
		fmt.Printf("  ● %-44s UPDATED — %s\n", shortPath(r.Path), truncQ(r.Reason, 90))
	case r.Status == "stale":
		fmt.Printf("  ○ %-44s stale (not written) — %s\n", shortPath(r.Path), truncQ(r.Reason, 80))
	case r.Status == "rejected":
		fmt.Printf("  ! %-44s %s\n", shortPath(r.Path), truncQ(r.Reason, 90))
	case verbose:
		fmt.Printf("    %-44s %s — %s\n", shortPath(r.Path), r.Status, truncQ(r.Reason, 70))
	}
}

func shortPath(p string) string { return strings.TrimPrefix(p, "facts/") }

func collectFiles(n gitloom.TreeNode, out *[]string) {
	if n.Kind == "file" && strings.HasSuffix(n.Path, ".md") {
		*out = append(*out, n.Path)
	}
	for _, c := range n.Children {
		collectFiles(c, out)
	}
}

func parseWhen(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// searchTerms splits a model-written query into terms worth searching.
//
// Stopwords and very short tokens are dropped: they match half the archive and
// bury the few messages that bear on the claim.
func searchTerms(q string) []string {
	stop := map[string]bool{
		"the": true, "and": true, "for": true, "was": true, "has": true,
		"still": true, "with": true, "from": true, "his": true, "her": true,
		"that": true, "this": true, "have": true, "been": true, "are": true,
		"per": true, "not": true, "any": true, "new": true, "old": true,
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.Fields(q) {
		w = strings.Trim(w, `.,"'()[]{}:;!?`)
		lw := strings.ToLower(w)
		if len(w) < 4 || stop[lw] || seen[lw] {
			continue
		}
		seen[lw] = true
		out = append(out, w)
		if len(out) >= 4 {
			break
		}
	}
	// Everything was a stopword: fall back to the raw query rather than
	// searching for nothing.
	if len(out) == 0 && strings.TrimSpace(q) != "" {
		out = []string{strings.TrimSpace(q)}
	}
	return out
}

// fetchMessages runs one wacli search. A failure returns nothing rather than
// an error: the audit degrades to less evidence, and "unverifiable" is the
// correct verdict when evidence cannot be gathered.
func fetchMessages(ctx context.Context, base string, q url.Values) []waMessage {
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/messages?"+q.Encode(), nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Messages []waMessage `json:"messages"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Messages
}

// shortJID trims a WhatsApp identifier to something a model can read without
// spending attention on the suffix.
func shortJID(j string) string {
	if i := strings.IndexAny(j, ":@"); i > 0 {
		return j[:i]
	}
	return j
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}
