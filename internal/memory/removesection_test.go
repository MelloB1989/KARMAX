package memory

import (
	"strings"
	"testing"
)

const doc = `Facts about the WhatsApp proxy.

## 2026-08-20 — first thing that happened

The first fact body.

## 2026-08-23 — second thing that happened

The second fact body.

## 2026-08-25 — third thing

The third fact body.`

// A memory is addressed as file.md#slug, and the merge pass could not delete a
// single one of them: Forget demanded a ".md" suffix, so it wrote consolidated
// facts and removed nothing, adding a memory on every run.
func TestRemoveSectionCutsOnlyItsOwnBlock(t *testing.T) {
	out, ok := RemoveSection(doc, "2026-08-23-second-thing-that-happened")
	if !ok {
		t.Fatal("the section should have matched")
	}
	if strings.Contains(out, "second fact body") {
		t.Error("the target section is still there")
	}
	for _, keep := range []string{"first fact body", "third fact body", "Facts about the WhatsApp proxy"} {
		if !strings.Contains(out, keep) {
			t.Errorf("removing one section destroyed %q", keep)
		}
	}
}

// The stored slug is a truncation of the header's slug — matching has to
// tolerate that or nothing is ever deletable.
func TestATruncatedSlugStillMatches(t *testing.T) {
	if _, ok := RemoveSection(doc, "2026-08-20-first-thing-that-happ"); !ok {
		t.Error("a truncated slug must still match its section")
	}
}

// A slug that names nothing must leave the document untouched.
func TestAnUnknownSlugChangesNothing(t *testing.T) {
	out, ok := RemoveSection(doc, "2026-01-01-never-happened")
	if ok {
		t.Error("reported a removal that did not happen")
	}
	if out != doc {
		t.Error("the document was modified for a slug that matched nothing")
	}
}

func TestRemovingTheLastSectionLeavesNothingToKeep(t *testing.T) {
	single := "## 2026-08-20 — only fact\n\nThe only body."
	out, ok := RemoveSection(single, "2026-08-20-only-fact")
	if !ok {
		t.Fatal("should have matched")
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected an empty remainder, got %q", out)
	}
}
