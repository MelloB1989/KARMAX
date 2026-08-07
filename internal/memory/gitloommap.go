package memory

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	gitloom "github.com/GitLoomHQ/gitloom-go/gitloom"
)

// Translating a KARMAX memory into a GitLoom one.
//
// The two models disagree in a productive way. KARMAX stores a flat row with a
// category and an importance; GitLoom stores a markdown file whose DIRECTORY is
// the topic and whose frontmatter carries confidence, dates, retrieval cues and
// relationships. So the mapping is not a field-by-field copy — it is a
// promotion: category becomes a place in a tree, importance becomes confidence,
// and the tags KARMAX only ever filtered on become cues that get embedded.
//
// Shared by the live write path and the one-time migration on purpose: a
// migration that files memories differently from how the daemon will file them
// tomorrow produces two disjoint stores wearing one name.

// topicFor maps a KARMAX category onto a GitLoom tier and topic directory.
//
// Tasks land in incidents/ rather than facts/ because that is what they are:
// time-bound, and the only tier GitLoom will ever expire. Everything else is a
// standing fact about the operator's world.
func topicFor(category string, when time.Time) string {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "relationship":
		return "facts/people"
	case "project":
		return "facts/projects"
	case "decision":
		return "facts/decisions"
	case "preference", "user_info":
		return "facts/operator"
	case "reference":
		return "facts/reference"
	case "feedback":
		return "facts/feedback"
	case "task":
		if when.IsZero() {
			when = time.Now()
		}
		return fmt.Sprintf("incidents/%04d/%02d", when.Year(), int(when.Month()))
	default:
		return "facts/context"
	}
}

// confidenceFor turns KARMAX's 1–4 importance into GitLoom's [0,1] confidence.
// A pinned memory is one the operator asserted rather than one an extractor
// guessed, so it is certain regardless of the level it was filed at.
func confidenceFor(importance int, pinned bool) float64 {
	if pinned {
		return 1.0
	}
	switch {
	case importance <= 1:
		return 0.35
	case importance == 2:
		return 0.6
	case importance == 3:
		return 0.85
	default:
		return 1.0
	}
}

// prefixRe strips the "[relationship][high] " KARMAX stamps on its own content.
// Redundant once the category is a directory and the importance is a
// confidence, and actively harmful as body text: it is the first thing BM25
// sees and it is identical across hundreds of memories.
var prefixRe = regexp.MustCompile(`^\s*(\[[a-z_]+\]\s*)+`)

// nonSlug is everything that cannot appear in a path segment.
var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-")
}

// genericTags are labels that describe the filing system rather than the
// subject, so they make useless slugs and useless cues.
var genericTags = map[string]bool{
	"relationship": true, "project": true, "task": true, "context": true,
	"decision": true, "preference": true, "user_info": true, "reference": true,
	"feedback": true, "memory": true, "reviewed": true,
	"high": true, "medium": true, "low": true, "critical": true,
	// Provenance, not subject: these say which loop wrote the memory, and 184
	// entries carry "loop" as their first tag. Treating it as the subject files
	// a quarter of the corpus under one name.
	"loop": true, "auto": true, "ingest": true, "summary": true, "digest": true,
}

// subjectOf picks what a memory is ABOUT, which becomes its filename.
//
// The first non-generic tag is the best signal KARMAX carries: its extractor
// consistently tags a memory with its subject first ("shiva", "campx",
// "truststrike"). Failing that, the opening words of the body are used, which
// is nearly as good because the extractor writes subject-first prose.
func subjectOf(e MemoryEntry) string {
	for _, t := range e.Tags {
		t = strings.TrimSpace(t)
		if t == "" || genericTags[strings.ToLower(t)] {
			continue
		}
		if s := slugify(t); s != "" {
			return s
		}
	}
	body := prefixRe.ReplaceAllString(e.Content, "")
	words := strings.Fields(body)
	if len(words) > 6 {
		words = words[:6]
	}
	if s := slugify(strings.Join(words, "-")); s != "" {
		return s
	}
	return "memory"
}

// cuesFor builds the retrieval keys that get embedded for semantic search.
//
// This is the single highest-leverage part of the mapping. KARMAX's tags were
// only ever filter labels — never searched, never embedded. As cues they become
// the phrasings a later question is matched against, which is how a memory
// about "Shiva Charan" is found by someone asking "who runs delivery at CampX".
func cuesFor(e MemoryEntry) []string {
	seen := map[string]bool{}
	var cues []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[strings.ToLower(s)] || len(cues) >= 5 {
			return
		}
		seen[strings.ToLower(s)] = true
		cues = append(cues, s)
	}
	for _, t := range e.Tags {
		if !genericTags[strings.ToLower(strings.TrimSpace(t))] {
			add(t)
		}
	}
	// The opening sentence is the extractor's own one-line statement of the
	// memory, which is close to how it would later be asked for.
	body := prefixRe.ReplaceAllString(e.Content, "")
	if i := strings.IndexAny(body, ".\n"); i > 12 {
		add(strings.TrimSpace(body[:i]))
	}
	return cues
}

// PathFor is the GitLoom path a KARMAX entry is filed at. Deterministic, so
// re-running a migration overwrites rather than duplicating.
//
// Two memories about the same subject deliberately return the SAME path: they
// belong in one file, as separate sections. See Merge.
func PathFor(e MemoryEntry, _ map[string]bool) string {
	return fmt.Sprintf("%s/%s.md", topicFor(e.Category, e.CreatedAt), subjectOf(e))
}

// sectionTitle names one entry's section within a merged file.
func sectionTitle(e MemoryEntry) string {
	body := strings.TrimSpace(prefixRe.ReplaceAllString(e.Content, ""))
	// The first clause is the extractor's own statement of the fact, which
	// makes a better header than a truncation at N characters would.
	cut := len(body)
	if i := strings.IndexAny(body, ".\n"); i > 0 && i < cut {
		cut = i
	}
	title := strings.TrimSpace(body[:cut])
	if len(title) > 90 {
		title = strings.TrimSpace(title[:90]) + "…"
	}
	if title == "" {
		title = "note"
	}
	// Headers cannot contain a newline and should not carry markdown that would
	// change the level.
	return strings.TrimLeft(strings.ReplaceAll(title, "\n", " "), "#> ")
}

// Merge folds every KARMAX entry that belongs to one subject into a single
// GitLoom memory whose ## headers are the individual facts.
//
// This is the difference between a dump and a migration. GitLoom indexes each
// header as its own addressable sub-node (path.md#slug), so one file per person
// or project — with a dated section per fact — gives retrieval something to
// point AT, and gives navigation a tree with real branches. Filing 685
// separate files instead, most of them hash-suffixed variants of the same
// forty subjects, would reproduce the flat pile the migration exists to leave.
//
// Entries must arrive oldest-first; sections are written in that order so a
// file reads as the history of its subject.
func Merge(path string, entries []MemoryEntry, related []string) gitloom.Memory {
	if len(entries) == 1 {
		return ToGitLoom(entries[0], path, related)
	}

	var (
		body    strings.Builder
		tags    []string
		cues    []string
		seenTag = map[string]bool{}
		seenCue = map[string]bool{}
		conf    float64
		latest  time.Time
	)
	for _, e := range entries {
		title := sectionTitle(e)
		body.WriteString("## ")
		if !e.CreatedAt.IsZero() {
			body.WriteString(e.CreatedAt.Format("2006-01-02") + " — ")
		}
		body.WriteString(title + "\n\n")
		body.WriteString(strings.TrimSpace(prefixRe.ReplaceAllString(e.Content, "")))
		body.WriteString("\n\n")

		for _, t := range append(append([]string{}, e.Tags...), e.Category) {
			t = strings.TrimSpace(strings.ToLower(t))
			if t != "" && !seenTag[t] {
				seenTag[t] = true
				tags = append(tags, t)
			}
		}
		for _, c := range cuesFor(e) {
			k := strings.ToLower(c)
			if !seenCue[k] && len(cues) < 5 {
				seenCue[k] = true
				cues = append(cues, c)
			}
		}
		if c := confidenceFor(e.Importance, e.Pinned); c > conf {
			conf = c
		}
		if e.CreatedAt.After(latest) {
			latest = e.CreatedAt
		}
	}

	m := gitloom.Memory{
		Path: path, Content: strings.TrimSpace(body.String()),
		Tags: tags, Confidence: conf, Cues: cues, Related: related,
	}
	// The most recent fact dates the file: retrieval orders by recency, and a
	// subject the operator discussed yesterday should not rank as if the
	// conversation stopped in March because that is when the file was started.
	if !latest.IsZero() {
		m.Date = latest.Format("2006-01-02")
	}
	return m
}

// AppendSection folds a new memory into the file that already holds its
// subject, rather than replacing it.
//
// A hosted write is a whole-file overwrite, so the live path cannot reuse the
// migration's merge: the first new fact about CampX would delete the forty
// already filed there. This reads what is stored, appends the new material as
// another section, and unions the frontmatter — which is what keeps one file
// per subject growing instead of fragmenting into dated near-duplicates.
//
// existing may be empty, which is the first-write case.
func AppendSection(existing string, incoming gitloom.Memory) gitloom.Memory {
	existing = strings.TrimSpace(existing)
	if existing == "" {
		return incoming
	}
	body := strings.TrimSpace(incoming.Content)
	if body == "" || strings.Contains(existing, body) {
		// Already recorded. Re-appending the same fact on every retry would
		// grow the file without adding anything to it.
		out := incoming
		out.Content = existing
		return out
	}

	var b strings.Builder
	b.WriteString(existing)
	b.WriteString("\n\n## ")
	if incoming.Date != "" {
		b.WriteString(incoming.Date + " — ")
	}
	title := body
	if i := strings.IndexAny(title, ".\n"); i > 0 {
		title = title[:i]
	}
	if len(title) > 90 {
		title = strings.TrimSpace(title[:90]) + "…"
	}
	b.WriteString(strings.TrimLeft(strings.ReplaceAll(title, "\n", " "), "#> "))
	b.WriteString("\n\n")
	b.WriteString(body)

	out := incoming
	out.Content = b.String()
	return out
}

// UnionMemories combines several writes to one path into a single document,
// so a batch carrying three new facts about one subject makes one file rather
// than three overwrites of each other.
func UnionMemories(ms []gitloom.Memory) gitloom.Memory {
	if len(ms) == 1 {
		return ms[0]
	}
	out := ms[0]
	for _, m := range ms[1:] {
		out = AppendSection(out.Content, gitloom.Memory{
			Path: m.Path, Content: m.Content, Date: m.Date,
			Tags: m.Tags, Confidence: m.Confidence, Cues: m.Cues, Related: m.Related,
		})
		out.Tags = unionStrings(ms[0].Tags, m.Tags, 24)
		out.Cues = unionStrings(ms[0].Cues, m.Cues, 5)
		out.Related = unionStrings(ms[0].Related, m.Related, 32)
		if m.Confidence > out.Confidence {
			out.Confidence = m.Confidence
		}
		if m.Date > out.Date {
			out.Date = m.Date // lexicographic works on YYYY-MM-DD
		}
	}
	return out
}

func unionStrings(a, b []string, max int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, max)
	for _, s := range append(append([]string{}, a...), b...) {
		k := strings.ToLower(strings.TrimSpace(s))
		if k == "" || seen[k] || len(out) >= max {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

// ToGitLoom converts one KARMAX entry into the memory GitLoom will store.
// related carries already-resolved GitLoom paths for this entry's links.
func ToGitLoom(e MemoryEntry, path string, related []string) gitloom.Memory {
	body := strings.TrimSpace(prefixRe.ReplaceAllString(e.Content, ""))

	tags := make([]string, 0, len(e.Tags)+1)
	seen := map[string]bool{}
	for _, t := range append(e.Tags, e.Category) {
		t = strings.TrimSpace(strings.ToLower(t))
		if t != "" && !seen[t] {
			seen[t] = true
			tags = append(tags, t)
		}
	}

	m := gitloom.Memory{
		Path:       path,
		Content:    body,
		Tags:       tags,
		Confidence: confidenceFor(e.Importance, e.Pinned),
		Cues:       cuesFor(e),
		Related:    related,
	}
	if !e.CreatedAt.IsZero() {
		// The date the memory is ABOUT. Without it a migration stamps six months
		// of history with today and every recency judgement is wrong.
		m.Date = e.CreatedAt.Format("2006-01-02")
	}
	// Only incidents can expire, and only they should: a task from March is
	// noise by August, while a fact about a person is not.
	if e.ExpiresAt != nil && strings.HasPrefix(path, "incidents/") {
		if d := time.Until(*e.ExpiresAt); d > 0 {
			m.TTL = fmt.Sprintf("%dd", int(d.Hours()/24)+1)
		}
	}
	return m
}
