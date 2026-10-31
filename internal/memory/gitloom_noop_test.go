package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every write makes the store re-upload the whole repository and its index, so
// one that changes nothing costs as much as one that does — and a loop that
// files the same line each run filled the disk with copies of an unchanged
// repository.

// Saying again what a document already holds sends nothing, whether the store
// has caught up with the first write or KARMAX only knows it from what it sent.
func TestWritingAFactAlreadyHeldSendsNothing(t *testing.T) {
	st := &slowStore{files: map[string]string{}, delay: 60 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)
	write := func(fact string) {
		t.Helper()
		if err := m.Write(MemoryEntry{Content: fact, Category: "project", Tags: []string{"subject"}}); err != nil {
			t.Fatal(err)
		}
	}

	write("alpha is first")
	write("bravo is second")
	write("alpha is first") // the store has not caught up: known from what was sent
	if n := st.posts.Load(); n != 2 {
		t.Errorf("%d writes reached the store before it caught up, want 2", n)
	}
	st.final("facts/projects/subject.md")
	write("bravo is second") // the store holds it now
	write("alpha is first")
	if n := st.posts.Load(); n != 2 {
		t.Errorf("%d writes reached the store, want 2", n)
	}
	got := st.final("facts/projects/subject.md")
	for _, f := range []string{"alpha is first", "bravo is second"} {
		if n := factCount(got, f); n != 1 {
			t.Errorf("%q is held %d times, want once:\n%s", f, n, got)
		}
	}
}

// factCount is how many times a document states a fact as a paragraph of its own.
func factCount(doc, fact string) int {
	n := 0
	for _, l := range strings.Split(doc, "\n") {
		if strings.TrimSpace(l) == fact {
			n++
		}
	}
	return n
}

// A fact said again does not make the document newer than its newest fact, and
// a write that only adds a tag keeps the document's date and confidence.
func TestAFoldThatAddsNoFactKeepsTheDocumentsDate(t *testing.T) {
	var mu sync.Mutex
	var posted []map[string]any
	earlier := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/memories"):
			writeJSON(w, map[string]any{
				"path": "facts/projects/s.md", "content": "First fact.\n\n## 2026-09-01 — second fact\n\nsecond fact",
				"user_tags": []string{"s", "project"}, "cues": []string{"s"}, "confidence": 0.85,
				"occurred_at": earlier.Unix(), "updated_at": time.Now().Unix() - 3600,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/memories":
			var body struct {
				Memories []map[string]any `json:"memories"`
			}
			_ = decodeJSON(r, &body)
			mu.Lock()
			posted = append(posted, body.Memories...)
			mu.Unlock()
			writeJSON(w, map[string]any{"written": 1})
		default:
			return false
		}
		return true
	})
	m, _ := managerWithGitLoom(t, srv.URL)
	said := time.Date(2026, 10, 30, 21, 0, 0, 0, time.UTC)

	// Nothing new at all: no write.
	if err := m.Write(MemoryEntry{Content: "second fact", Category: "project", Tags: []string{"s"}, Importance: 1, OccurredAt: said}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(posted) != 0 {
		t.Errorf("a fact the document already holds was written again: %v", posted)
	}
	mu.Unlock()

	// Only a new tag: written, but neither dated by this write nor lowered to its confidence.
	if err := m.Write(MemoryEntry{Content: "second fact", Category: "project", Tags: []string{"s", "person:ab"}, Importance: 1, OccurredAt: said}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 {
		t.Fatalf("%d writes for a new tag, want 1", len(posted))
	}
	mem := posted[0]
	if !strings.Contains(strings.Join(toStrings(mem["tags"]), ","), "person:ab") {
		t.Errorf("the new tag was not written: %v", mem["tags"])
	}
	if got, _ := mem["occurred_at"].(float64); int64(got) != earlier.Unix() {
		t.Errorf("occurred_at = %v, want the document's own %d: no fact was added", mem["occurred_at"], earlier.Unix())
	}
	if got, _ := mem["confidence"].(float64); got != 0.85 {
		t.Errorf("confidence = %v, want 0.85: a repeated fact does not make the document less certain", mem["confidence"])
	}
}

// Rewriting a memory with the text it already has, or correcting it with what
// it already says, sends nothing.
func TestUpdatingOrCorrectingToWhatIsThereSendsNothing(t *testing.T) {
	var posts atomic.Int32
	const doc = "Basil is watered on Sundays.\n\n## 2026-10-30 — Basil gets fed monthly\n\nBasil gets fed monthly."
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/memories"):
			writeJSON(w, map[string]any{"path": "facts/operator/basil.md", "content": doc,
				"user_tags": []string{"basil"}, "confidence": 0.6, "updated_at": time.Now().Unix() - 3600})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/memories":
			posts.Add(1)
			writeJSON(w, map[string]any{"written": 1})
		default:
			return false
		}
		return true
	})
	m, _ := managerWithGitLoom(t, srv.URL)
	ctx := context.Background()

	if err := m.Update(ctx, "facts/operator/basil.md", doc+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Correct(ctx, "facts/operator/basil.md", "Basil gets fed monthly."); err != nil {
		t.Fatal(err)
	}
	if n := posts.Load(); n != 0 {
		t.Errorf("%d writes changed nothing, want 0", n)
	}
}

// A fact forgotten and then said again is written again — the document as
// KARMAX knows it no longer holds it, whatever an older copy says.
func TestAFactForgottenAndSaidAgainIsWrittenAgain(t *testing.T) {
	const lag = 300 * time.Millisecond
	st := &slowStore{files: map[string]string{}, delay: 20 * time.Millisecond, lag: lag}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)
	write := func(fact string) {
		t.Helper()
		if err := m.Write(MemoryEntry{Content: fact, Category: "project", Tags: []string{"subject"}}); err != nil {
			t.Fatal(err)
		}
	}
	slugOf := func(doc, word string) string {
		for _, l := range strings.Split(doc, "\n") {
			if strings.HasPrefix(l, "## ") && strings.Contains(l, word) {
				return serverSlug(strings.TrimPrefix(l, "## "))
			}
		}
		t.Fatalf("no section for %q:\n%s", word, doc)
		return ""
	}

	write("alpha is first")
	write("bravo is second")
	write("charlie is third")
	doc := st.final("facts/projects/subject.md")
	time.Sleep(lag + 100*time.Millisecond)

	if err := m.Forget("facts/projects/subject.md#" + slugOf(doc, "bravo")); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	write("bravo is second") // said again: it is news once more
	if got := st.final("facts/projects/subject.md"); !strings.Contains(got, "bravo is second") {
		t.Fatalf("the fact said again was not written:\n%s", got)
	}
	// Forgetting another fact of the document leaves the one said again.
	if err := m.Forget("facts/projects/subject.md#" + slugOf(doc, "charlie")); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	write("delta is fourth")
	got := st.final("facts/projects/subject.md")
	if strings.Contains(got, "charlie") {
		t.Errorf("the forgotten fact came back:\n%s", got)
	}
	for _, want := range []string{"alpha is first", "bravo is second", "delta is fourth"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q was lost:\n%s", want, got)
		}
	}
}

// A fact folded onto a document the store has not caught up with keeps the
// expiry the document was written with: KARMAX's own copy of it carries one.
func TestAQuickSecondFactKeepsAnIncidentsExpiry(t *testing.T) {
	st := &slowStore{files: map[string]string{}, delay: 60 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)
	aug := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	expires := time.Now().Add(10 * 24 * time.Hour)

	if err := m.Write(MemoryEntry{Content: "Demo with Priya needs a new date.", Category: "task", Tags: []string{"priya"},
		ExpiresAt: &expires, OccurredAt: aug}); err != nil {
		t.Fatal(err)
	}
	if err := m.Write(MemoryEntry{Content: "Priya moved the demo to Friday.", Category: "task", Tags: []string{"priya"},
		OccurredAt: aug}); err != nil {
		t.Fatal(err)
	}
	const path = "incidents/2026/08/priya.md"
	if got := st.final(path); !strings.Contains(got, "moved the demo") {
		t.Fatalf("the second fact was not written:\n%s", got)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if ttl := st.meta[path].TTL; ttl != "10d" && ttl != "11d" {
		t.Errorf("ttl = %q, want the ten days the incident had left", ttl)
	}
}
