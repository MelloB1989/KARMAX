package memory

import (
	gitloom "github.com/GitLoomHQ/gitloom-go/gitloom"
	"strings"
	"testing"
	"time"
)

func entry(category, content string, tags []string, importance int) MemoryEntry {
	return MemoryEntry{
		ID: "e1", Category: category, Content: content, Tags: tags,
		Importance: importance, CreatedAt: time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC),
	}
}

func TestCategoryBecomesATopicDirectory(t *testing.T) {
	// The directory IS the topic in GitLoom, so a flat category column has to
	// become a place in the tree or navigation has nothing to navigate.
	cases := map[string]string{
		"relationship": "facts/people/",
		"project":      "facts/projects/",
		"decision":     "facts/decisions/",
		"preference":   "facts/operator/",
		"user_info":    "facts/operator/",
		"":             "facts/context/",
	}
	for category, wantPrefix := range cases {
		got := PathFor(entry(category, "Siva runs delivery at CampX.", []string{"siva"}, 2), nil)
		if !strings.HasPrefix(got, wantPrefix) {
			t.Errorf("category %q filed at %q, want under %q", category, got, wantPrefix)
		}
		if !strings.HasSuffix(got, ".md") {
			t.Errorf("path %q must end in .md", got)
		}
	}
}

func TestTasksBecomeExpirableIncidents(t *testing.T) {
	// Tasks are the one category that is time-bound, and incidents/ is the only
	// tier GitLoom will ever expire. Filing them under facts/ makes March's
	// to-do list permanent.
	got := PathFor(entry("task", "Send the CampX invoice.", []string{"campx"}, 3), nil)
	if !strings.HasPrefix(got, "incidents/2026/07/") {
		t.Errorf("task filed at %q, want a date-sharded incident", got)
	}
}

func TestImportanceBecomesConfidence(t *testing.T) {
	if confidenceFor(1, false) >= confidenceFor(4, false) {
		t.Error("higher importance must map to higher confidence")
	}
	// A pinned memory is one the operator asserted, not one an extractor
	// guessed, so it is certain whatever level it was filed at.
	if confidenceFor(1, true) != 1.0 {
		t.Errorf("pinned confidence = %v, want 1.0", confidenceFor(1, true))
	}
	for imp := 1; imp <= 4; imp++ {
		if c := confidenceFor(imp, false); c < 0 || c > 1 {
			t.Errorf("importance %d gave confidence %v, outside [0,1]", imp, c)
		}
	}
}

func TestSubjectDrivesTheFilename(t *testing.T) {
	e := entry("relationship", "Shiva Charan is aware of KARMAX.",
		[]string{"shiva", "karmax", "relationship"}, 3)
	got := PathFor(e, nil)
	if got != "facts/people/shiva.md" {
		t.Errorf("path = %q, want facts/people/shiva.md", got)
	}

	// Tags that name the filing system rather than the subject make useless
	// filenames — every relationship memory would land on the same file.
	// "loop" is the worst of them: 184 entries carry it first.
	generic := entry("relationship", "Aymaan built the optimizations.",
		[]string{"relationship", "high", "loop", "aymaan"}, 2)
	if p := PathFor(generic, nil); !strings.Contains(p, "aymaan") {
		t.Errorf("path %q was built from a generic tag instead of the subject", p)
	}
}

func TestOneSubjectIsOneFile(t *testing.T) {
	e := entry("project", "TrustStrike is the VAPT product.", []string{"truststrike"}, 3)
	// Deterministic: re-running a migration must overwrite, not duplicate.
	if PathFor(e, nil) != PathFor(e, nil) {
		t.Error("PathFor is not deterministic")
	}

	// Two memories about one subject deliberately share a path — they are
	// folded into one file as separate sections. Hash-suffixing them instead
	// scattered 684 entries across 477 near-duplicate files and gave
	// navigation nothing to navigate.
	other := entry("project", "TrustStrike moved to Fargate for VAPT extraction.", []string{"truststrike"}, 3)
	if PathFor(e, nil) != PathFor(other, nil) {
		t.Error("two memories about one subject should share a file")
	}
}

func TestMergeMakesAddressableSections(t *testing.T) {
	older := entry("project", "TrustStrike is the VAPT product.", []string{"truststrike"}, 2)
	older.CreatedAt = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	newer := entry("project", "TrustStrike moved VAPT extraction to Fargate.", []string{"truststrike", "aws"}, 3)

	m := Merge("facts/projects/truststrike.md", []MemoryEntry{older, newer}, []string{"uses: facts/projects/aws.md"})

	// Each fact becomes its own ## header, which GitLoom indexes as a separately
	// addressable sub-node — that is what lets retrieval cite a section.
	if n := strings.Count(m.Content, "## "); n != 2 {
		t.Errorf("%d sections, want 2:\n%s", n, m.Content)
	}
	for _, want := range []string{"2026-06-01", "Fargate", "VAPT product"} {
		if !strings.Contains(m.Content, want) {
			t.Errorf("merged body lost %q", want)
		}
	}
	// The file is dated by its most recent fact: a subject discussed yesterday
	// must not rank as if the conversation stopped when the file was started.
	if m.Date != newer.CreatedAt.Format("2006-01-02") {
		t.Errorf("date = %q, want the newest entry's", m.Date)
	}
	// Confidence is the highest claim in the file, not the first.
	if m.Confidence != confidenceFor(3, false) {
		t.Errorf("confidence = %v, want the strongest entry's", m.Confidence)
	}
	if len(m.Related) != 1 {
		t.Errorf("related = %v", m.Related)
	}
}

func TestAppendSectionNeverDropsWhatIsStored(t *testing.T) {
	existing := "## 2026-06-01 — TrustStrike is the VAPT product\n\nTrustStrike is the VAPT product."
	incoming := gitloom.Memory{
		Path: "facts/projects/truststrike.md", Date: "2026-08-01",
		Content: "TrustStrike now runs extraction on Fargate.",
	}

	got := AppendSection(existing, incoming)
	// A hosted write replaces the whole file, so anything this drops is gone.
	if !strings.Contains(got.Content, "VAPT product") {
		t.Errorf("appending destroyed the existing content:\n%s", got.Content)
	}
	if !strings.Contains(got.Content, "Fargate") {
		t.Error("the new memory was not appended")
	}
	if n := strings.Count(got.Content, "## "); n != 2 {
		t.Errorf("%d sections, want 2", n)
	}

	// Re-delivering the same memory must leave the file untouched. The outbox
	// retries on every failure, so without this a flaky network appends the
	// same fact once per attempt.
	twice := AppendSection(got.Content, incoming)
	if twice.Content != got.Content {
		t.Errorf("a retried delivery changed the file:\n%s", twice.Content)
	}
}

func TestTagsBecomeCues(t *testing.T) {
	// The highest-leverage part of the mapping: KARMAX's tags were filter
	// labels that were never searched. As cues they get embedded, which is how
	// a memory is found by a question that shares none of its words.
	e := entry("relationship", "Siva Charan runs day-to-day delivery at CampX. He is the main contact.",
		[]string{"siva", "campx", "relationship"}, 3)
	cues := cuesFor(e)
	if len(cues) == 0 {
		t.Fatal("no cues were derived")
	}
	for _, c := range cues {
		if genericTags[strings.ToLower(c)] {
			t.Errorf("cue %q describes the filing system, not the subject", c)
		}
	}
	if len(cues) > 5 {
		t.Errorf("%d cues; GitLoom expects 2-5", len(cues))
	}
}

func TestToGitLoomStripsKarmaxFormatting(t *testing.T) {
	e := entry("relationship", "[relationship][high] Shiva Charan is aware of KARMAX.",
		[]string{"shiva"}, 3)
	m := ToGitLoom(e, "facts/people/shiva.md", []string{"colleague: facts/people/kartik.md"})

	// The prefix is identical across hundreds of memories and is the first
	// thing BM25 sees, so leaving it in actively degrades ranking.
	if strings.HasPrefix(m.Content, "[") {
		t.Errorf("content still carries the category prefix: %q", m.Content)
	}
	if !strings.Contains(m.Content, "Shiva Charan") {
		t.Errorf("stripping removed real content: %q", m.Content)
	}
	// The date the memory is ABOUT. Losing it stamps six months of history
	// with today and every recency judgement afterwards is wrong.
	if m.Date != "2026-07-19" {
		t.Errorf("date = %q, want 2026-07-19", m.Date)
	}
	if len(m.Related) != 1 {
		t.Errorf("related = %v, want the one link", m.Related)
	}
	if m.Confidence != confidenceFor(3, false) {
		t.Errorf("confidence = %v", m.Confidence)
	}
}

func TestOnlyIncidentsCarryATTL(t *testing.T) {
	exp := time.Now().Add(72 * time.Hour)
	e := entry("task", "Ship the APK.", []string{"ev"}, 3)
	e.ExpiresAt = &exp

	if m := ToGitLoom(e, "incidents/2026/07/ship-apk.md", nil); m.TTL == "" {
		t.Error("an expiring task filed as an incident should carry its TTL")
	}
	// A fact about a person must not silently evaporate because the row it came
	// from happened to have an expiry set.
	if m := ToGitLoom(e, "facts/people/someone.md", nil); m.TTL != "" {
		t.Errorf("a facts/ memory must not carry a TTL, got %q", m.TTL)
	}
}
