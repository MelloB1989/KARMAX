package memory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

The docs site is still building.`

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
	for _, kept := range []string{"Lead about the chat.", "The APK ask is still open.", "The docs site is still building."} {
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

// Correcting ONE fact replaces it within its document: the corrected statement
// is added, the old section goes, and every other fact stays.
func TestCorrectingASectionReplacesJustThatFact(t *testing.T) {
	st := &slowStore{files: map[string]string{"facts/context/chat.md": chatDoc}, delay: 40 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)

	replaced, err := m.Correct(context.Background(), callSection, "Quinn's call with Omar moved to Saturday 2026-10-10.")
	if err != nil {
		t.Fatalf("Correct: %v", err)
	}
	if !replaced {
		t.Error("a single fact was not reported replaced")
	}
	got := st.final("facts/context/chat.md")
	if strings.Contains(got, "They agreed on Friday") {
		t.Errorf("the corrected fact is still there:\n%s", got)
	}
	for _, want := range []string{"moved to Saturday", "The APK ask is still open.", "The docs site is still building.", "Lead about the chat."} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing:\n%s", want, got)
		}
	}
	if _, ok := st.files["facts/context/chat.md#2026-10-08-quinns-call-with-omar-at-1947-ist"]; ok {
		t.Error("the correction was written to a file named after the section")
	}
}

// "Nothing there" and "a document of several facts" both used to come back as
// not-removed, so a caller told the operator a memory was kept for holding
// several facts when it did not exist at all.
func TestForgetFactOfNothingIsNotStored(t *testing.T) {
	st := &slowStore{files: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)

	removed, err := m.ForgetFact(context.Background(), "facts/people/nobody.md")
	if removed || !errors.Is(err, ErrNotStored) {
		t.Fatalf("ForgetFact of nothing = %v, %v; want ErrNotStored", removed, err)
	}
}

// An incident keeps its expiry when another fact folds into it. Rewritten
// without it, a time-bound task became permanent the first time anything else
// about it was written.
func TestAFoldKeepsAnIncidentsExpiry(t *testing.T) {
	var mu sync.Mutex
	var ttl string
	expires := time.Now().Add(10 * 24 * time.Hour)
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]any{"path": "incidents/2026/08/priya.md", "content": "Demo with Priya needs a new date.",
				"expires_at": expires.Unix(), "updated_at": time.Now().Unix() - 3600})
		case http.MethodPost:
			var body struct {
				Memories []struct {
					TTL string `json:"ttl"`
				} `json:"memories"`
			}
			_ = decodeJSON(r, &body)
			mu.Lock()
			if len(body.Memories) > 0 {
				ttl = body.Memories[0].TTL
			}
			mu.Unlock()
			writeJSON(w, map[string]any{"written": 1})
		default:
			return false
		}
		return true
	})
	m, _ := managerWithGitLoom(t, srv.URL)
	if err := m.Write(MemoryEntry{Content: "Priya moved the demo to Friday.", Category: "task", Tags: []string{"priya"},
		OccurredAt: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if ttl != "10d" && ttl != "11d" {
		t.Errorf("ttl written = %q, want the ten days the incident had left", ttl)
	}
}

// The store's readers do not all see a change at once: one can answer that a
// forgotten fact is gone while another still serves the document from before.
// Folding the next write onto that older copy carried the fact straight back.
func TestAForgetIsNotUndoneByAStaleRead(t *testing.T) {
	const lag = 300 * time.Millisecond
	st := &slowStore{files: map[string]string{}, delay: 20 * time.Millisecond, lag: lag}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)
	// Every reader holds what was written, so the stale one serves the fact
	// about to be forgotten.
	settle := func() { time.Sleep(lag + 100*time.Millisecond) }
	write := func(fact, subject string) {
		t.Helper()
		if err := m.Write(MemoryEntry{Content: fact, Category: "project", Tags: []string{subject}}); err != nil {
			t.Fatal(err)
		}
	}

	// One fact of several.
	write("alpha is first", "subject")
	write("bravo met Quinn at 19:47 IST", "subject")
	doc := st.final("facts/projects/subject.md")
	settle()
	slug := ""
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(l, "## ") && strings.Contains(l, "bravo") {
			slug = serverSlug(strings.TrimPrefix(l, "## "))
		}
	}
	if err := m.Forget("facts/projects/subject.md#" + slug); err != nil {
		t.Fatalf("Forget section: %v", err)
	}
	write("charlie is third", "subject")
	if got := st.final("facts/projects/subject.md"); strings.Contains(got, "bravo") || !strings.Contains(got, "charlie") || !strings.Contains(got, "alpha") {
		t.Errorf("after forgetting a section and writing again:\n%s", got)
	}

	// A whole document.
	write("delta is alone", "other")
	st.final("facts/projects/other.md")
	settle()
	if err := m.Forget("facts/projects/other.md"); err != nil {
		t.Fatalf("Forget document: %v", err)
	}
	write("echo is new", "other")
	if got := st.final("facts/projects/other.md"); strings.Contains(got, "delta") || !strings.Contains(got, "echo") {
		t.Errorf("after forgetting a document and writing again:\n%s", got)
	}
}

// A document's first fact is written as its opening paragraph, with no header;
// the next ones as sections. Counting only headers took a document of two facts
// for one, and forgetting "it" took both.
func TestTheOpeningParagraphIsAFact(t *testing.T) {
	for doc, want := range map[string]int{
		"Basil is watered on Sundays.": 1,
		"Basil is watered on Sundays.\n\n## 2026-10-30 — Basil is watered on Wednesdays\n\nWednesdays.": 2,
		"## a\n\none\n\n## b\n\ntwo": 2,
		"\n\n## a\n\none": 1,
	} {
		if got := CountFacts(doc); got != want {
			t.Errorf("CountFacts(%q) = %d, want %d", doc, got, want)
		}
	}
}
