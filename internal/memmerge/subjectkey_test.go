package memmerge

import (
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
)

func key(path string) string {
	return subjectKey(store.StoredMemoryEntry{ID: path, Content: "x"})
}

// The real cluster: nineteen facts about one dropped collaboration, filed by
// the store under six different folders. Grouping by category left them in one
// bucket of ~1900 the pass could only sample; they have to land together.
func TestOneSubjectClustersAcrossFolders(t *testing.T) {
	paths := []string{
		"facts/completed-tasks/shravan-kumar-scripting-collaboration-dropped-a9bd68b8.md",
		"facts/decisions/shravan-kumar-scripting-collaboration-officially-dropped.md",
		"facts/decisions/shravan.md",
		"facts/dropped-collaborations/shravan-nalacharla-scripting-collaboration-dropped-dc17b4a2.md",
		"facts/pending-task/shravan-scripting-collaboration-dropped-95edf007.md",
		"facts/people/shravan-kumar-scripting-collab-dropped-aug-11-d7f18387.md",
	}
	want := key(paths[0])
	if want == "" {
		t.Fatal("subject key must not be empty")
	}
	for _, p := range paths {
		if got := key(p); got != want {
			t.Errorf("%s -> %q, want %q — the cluster is split", p, got, want)
		}
	}
}

// A leading ISO date is filing metadata, not the subject.
func TestLeadingDateIsNotTheSubject(t *testing.T) {
	a := key("facts/context/2026-08-13-campx-prd-overdue.md")
	b := key("facts/projects/campx-prd-status.md")
	if a != b {
		t.Errorf("date-prefixed %q should match %q", a, b)
	}
}

// Two genuinely different people must not be merged.
func TestDifferentSubjectsStaySeparate(t *testing.T) {
	if key("facts/people/shravan-kumar-dropped.md") == key("facts/people/princi-jain-colleague.md") {
		t.Error("different people must not share a subject key")
	}
	if key("facts/people/nikhil-gunda-hardware.md") == key("facts/people/krishna-vector-coo.md") {
		t.Error("Nikhil and Krishna must never cluster together")
	}
}

// A section path addresses a fact inside a file; it belongs with its file.
func TestSectionPathsClusterWithTheirFile(t *testing.T) {
	f := key("facts/projects/campx.md")
	s := key("facts/projects/campx.md#2026-08-20-hosting-plan-for-student-projects")
	if f != s {
		t.Errorf("section %q should cluster with file %q", s, f)
	}
}
