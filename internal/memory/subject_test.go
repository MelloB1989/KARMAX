package memory

import "testing"

// One subject must map to one file, however the fact is worded. It did not:
// the filename came from the first six words of the body, so every rewording
// minted a new file and AppendSection never got the chance to fold facts into
// the document that already held the subject. Nineteen files existed for one
// dropped collaboration.
func TestOneSubjectOneFileHoweverItIsWorded(t *testing.T) {
	same := []string{
		"Shravan Kumar Nalacharla's scripting collaboration was officially dropped on Aug 11",
		"Shravan collaboration dropped, no further follow-up needed",
		"Kartik decided to drop the Shravan scripting collab",
	}
	want := subjectOf(MemoryEntry{Content: same[0]})
	for _, c := range same[1:] {
		if got := subjectOf(MemoryEntry{Content: c}); got != want {
			t.Errorf("subjectOf(%.40s...) = %q, want %q", c, got, want)
		}
	}
}

// A tag is the primary route and must win over the body — the extractor tags a
// fact with its subject first.
func TestATagNamesTheSubject(t *testing.T) {
	e := MemoryEntry{Content: "Some long sentence about many things", Tags: []string{"campx", "high"}}
	if got := subjectOf(e); got != "campx" {
		t.Errorf("got %q, want the first non-generic tag", got)
	}
}

// Filing-system labels are not subjects.
func TestGenericTagsAreSkipped(t *testing.T) {
	e := MemoryEntry{Content: "Krishna is the Vector COO", Tags: []string{"high", "context", "krishna"}}
	if got := subjectOf(e); got != "krishna" {
		t.Errorf("got %q, want %q", got, "krishna")
	}
}

// Two different people must not share a file.
func TestDifferentSubjectsGetDifferentFiles(t *testing.T) {
	a := subjectOf(MemoryEntry{Content: "Nikhil Gunda is a hardware collaborator"})
	b := subjectOf(MemoryEntry{Content: "Krishna is the Vector COO based in the US"})
	if a == b {
		t.Errorf("Nikhil and Krishna must not share a subject file (both %q)", a)
	}
}
