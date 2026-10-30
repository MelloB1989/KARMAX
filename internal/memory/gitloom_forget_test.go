package memory

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A document as KARMAX files it, with the punctuation real headers carry. The
// slug the store addresses the middle section by is "…-1947-ist"; turning the
// colon into a hyphen instead ("…-19-47-ist") is how forgets used to miss.
const chatDoc = `Lead about the chat.

## 2026-10-07 — (chat-sweep, 7 Oct 10:30) 1203630000@g.us [flagged]: no new messages

The APK ask is still open.

## 2026-10-08 — Quinn's call with Omar at 19:47 IST

They agreed on Friday.

## 2026-10-09 — Fleet at 14:00: four tasks working

ZeroDocs is still building.`

const callSection = "facts/context/chat.md#2026-10-08-quinns-call-with-omar-at-1947-ist"

func TestForgettingASectionHasTheStoreCutIt(t *testing.T) {
	st := &slowStore{files: map[string]string{"facts/context/chat.md": chatDoc}, delay: 50 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)

	if err := m.Forget(callSection); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	got := st.final("facts/context/chat.md")
	if strings.Contains(got, "They agreed on Friday") {
		t.Errorf("the section is still there:\n%s", got)
	}
	for _, kept := range []string{"Lead about the chat.", "The APK ask is still open.", "ZeroDocs is still building."} {
		if !strings.Contains(got, kept) {
			t.Errorf("forgetting one section took %q with it:\n%s", kept, got)
		}
	}
}

// Reporting success for a forget that matched nothing is how a merge pass
// claimed thousands of deletions while the store kept every duplicate.
func TestForgettingASectionThatIsNotThereIsAnError(t *testing.T) {
	st := &slowStore{files: map[string]string{"facts/context/chat.md": chatDoc}}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)

	err := m.Forget("facts/context/chat.md#2026-10-08-quinn-s-call-with-omar-at-19-47-ist")
	if !errors.Is(err, ErrNotStored) {
		t.Fatalf("Forget of an unknown section = %v, want ErrNotStored", err)
	}
	if got := st.final("facts/context/chat.md"); got != chatDoc {
		t.Errorf("a refused forget changed the document:\n%s", got)
	}
}

func TestForgettingADocumentThatIsNotThereIsAnError(t *testing.T) {
	st := &slowStore{files: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)

	if err := m.Forget("facts/people/nobody.md"); !errors.Is(err, ErrNotStored) {
		t.Fatalf("Forget of an unknown document = %v, want ErrNotStored", err)
	}
}

// The next write to a document folds onto what is stored. Read before the
// forget has landed — or from what KARMAX itself last wrote there — it carries
// the forgotten fact straight back.
func TestAWriteAfterAForgetDoesNotBringTheSectionBack(t *testing.T) {
	st := &slowStore{files: map[string]string{}, delay: 80 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)

	for _, f := range []string{"alpha is first", "bravo met Quinn at 19:47 IST"} {
		if err := m.Write(MemoryEntry{Content: f, Category: "project", Tags: []string{"subject"}}); err != nil {
			t.Fatal(err)
		}
	}
	doc := st.final("facts/projects/subject.md")
	slug := ""
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(l, "## ") && strings.Contains(l, "bravo") {
			slug = serverSlug(strings.TrimPrefix(l, "## "))
		}
	}
	if slug == "" {
		t.Fatalf("no section for the second fact:\n%s", doc)
	}

	if err := m.Forget("facts/projects/subject.md#" + slug); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if err := m.Write(MemoryEntry{Content: "charlie is third", Category: "project", Tags: []string{"subject"}}); err != nil {
		t.Fatal(err)
	}
	got := st.final("facts/projects/subject.md")
	if strings.Contains(got, "bravo") {
		t.Errorf("the forgotten fact came back with the next write:\n%s", got)
	}
	for _, kept := range []string{"alpha is first", "charlie is third"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q was lost:\n%s", kept, got)
		}
	}
}
