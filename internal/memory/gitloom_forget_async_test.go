package memory

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A forget returns once the store has accepted it. It used to wait — up to the
// thirty-second call timeout — for the store to apply it, and live ingest takes
// about two minutes, so every forget paid the whole thirty seconds: a merge pass
// of thirty-odd forgets spent most of its run waiting. What KARMAX forgot is
// kept in its pending record until the store shows it, which is what keeps the
// next write and the next read from bringing it back.

func slowForgetManager(t *testing.T, st *slowStore) *Manager {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)
	return m
}

// sectionHandle is the store's handle for the section whose header mentions fact.
func sectionHandle(t *testing.T, path, doc, fact string) string {
	t.Helper()
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(l, "## ") && strings.Contains(l, fact) {
			return path + "#" + serverSlug(strings.TrimPrefix(l, "## "))
		}
	}
	t.Fatalf("no section for %q in:\n%s", fact, doc)
	return ""
}

func TestAForgetDoesNotWaitForTheStoreToApplyIt(t *testing.T) {
	st := &slowStore{files: map[string]string{"facts/context/chat.md": chatDoc}, delay: 2 * time.Second}
	m := slowForgetManager(t, st)

	began := time.Now()
	if err := m.Forget(callSection); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if took := time.Since(began); took > 500*time.Millisecond {
		t.Errorf("the forget waited %v for the store to apply it", took)
	}
	if got := st.final("facts/context/chat.md"); strings.Contains(got, "They agreed on Friday") {
		t.Errorf("the forget was never applied:\n%s", got)
	}
}

// Forgetting what KARMAX has already forgotten is ErrNotStored, even before
// the store has applied the first forget.
func TestForgettingWhatIsAlreadyForgottenIsNotStored(t *testing.T) {
	st := &slowStore{files: map[string]string{"facts/context/chat.md": chatDoc}, delay: time.Second}
	m := slowForgetManager(t, st)

	if err := m.Forget(callSection); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if err := m.Forget(callSection); !errors.Is(err, ErrNotStored) {
		t.Errorf("forgetting the same section again = %v, want ErrNotStored", err)
	}
	if removed, err := m.ForgetFact(t.Context(), callSection); removed || !errors.Is(err, ErrNotStored) {
		t.Errorf("ForgetFact of the same section again = %v, %v; want ErrNotStored", removed, err)
	}
	if err := m.Forget("facts/context/chat.md"); err != nil {
		t.Fatalf("Forget document: %v", err)
	}
	if err := m.Forget("facts/context/chat.md"); !errors.Is(err, ErrNotStored) {
		t.Errorf("forgetting the same document again = %v, want ErrNotStored", err)
	}
	if got := st.final("facts/context/chat.md"); got != "" {
		t.Errorf("the document is still stored:\n%s", got)
	}
}

// A write straight after a forget the store has not applied yet does not
// carry the forgotten fact back.
func TestAWriteStraightAfterAForgetDoesNotBringItBack(t *testing.T) {
	st := &slowStore{files: map[string]string{}, delay: 300 * time.Millisecond}
	m := slowForgetManager(t, st)
	path := "facts/projects/subject.md"

	writeFact(t, m, "alpha is first", "subject")
	writeFact(t, m, "bravo met Quinn at 19:47 IST", "subject")
	doc := st.final(path)
	if err := m.Forget(sectionHandle(t, path, doc, "bravo")); err != nil {
		t.Fatal(err)
	}
	writeFact(t, m, "charlie is third", "subject")
	got := st.final(path)
	if strings.Contains(got, "bravo") {
		t.Errorf("the forgotten fact came back with the next write:\n%s", got)
	}
	for _, f := range []string{"alpha is first", "charlie is third"} {
		if !strings.Contains(got, f) {
			t.Errorf("%q was lost:\n%s", f, got)
		}
	}
}

// What KARMAX reports forgotten, it does not recall: not while the store is
// still applying the forget, and not from a reader serving a copy from before.
func TestWhatWasForgottenIsNotRecalled(t *testing.T) {
	const lag = 400 * time.Millisecond
	st := &slowStore{files: map[string]string{}, delay: 50 * time.Millisecond, lag: lag}
	m := slowForgetManager(t, st)
	settle := func() { time.Sleep(lag + 100*time.Millisecond) }
	path, other := "facts/projects/subject.md", "facts/projects/other.md"

	writeFact(t, m, "alpha is first", "subject")
	writeFact(t, m, "bravo met Quinn at 19:47 IST", "subject")
	writeFact(t, m, "delta is alone", "other")
	doc := st.final(path)
	st.final(other)
	settle()

	if err := m.Forget(sectionHandle(t, path, doc, "bravo")); err != nil {
		t.Fatal(err)
	}
	if err := m.Forget(other); err != nil {
		t.Fatal(err)
	}
	recalled := func(when string) {
		t.Helper()
		for _, word := range []string{"bravo", "delta"} {
			res, err := m.Search(word, 10)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range res {
				if strings.Contains(r.Entry.Content, word) {
					t.Errorf("%s, a search for %q recalled %s:\n%s", when, word, r.Entry.ID, r.Entry.Content)
				}
			}
		}
		listed, err := m.Recent(50)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range listed {
			if strings.Contains(e.Content, "bravo") || strings.Contains(e.Content, "delta") {
				t.Errorf("%s, the listing still holds %s:\n%s", when, e.ID, e.Content)
			}
		}
	}
	recalled("before the store applied the forgets")
	st.final(path)
	recalled("while a reader still serves the copy from before")
}
