package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The live store takes about two minutes to ingest a write — embedding alone
// is two — and its readers serve snapshots of different ages. These tests run
// the pending cache on a clock they move, so "three minutes later" is three
// minutes to the cache while the fake store lags in milliseconds.

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// onClock serves a slow store to a manager whose pending cache reads clock.
func onClock(t *testing.T, st *slowStore) (*Manager, *gitloomBackend, *testClock) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)
	m.mu.Lock()
	g := m.remote
	m.mu.Unlock()
	clock := &testClock{t: time.Now()}
	g.now = clock.now
	return m, g, clock
}

func writeFact(t *testing.T, m *Manager, fact, subject string) {
	t.Helper()
	if err := m.Write(MemoryEntry{Content: fact, Category: "project", Tags: []string{subject}}); err != nil {
		t.Fatal(err)
	}
}

// A write is trusted over the store's copy until the store shows it, however
// long ingest takes. Trusted for a fixed two minutes, a read just after them
// still found the store without the write, and the next fact folded onto that
// and replaced it.
func TestAWriteIsTrustedUntilTheStoreShowsIt(t *testing.T) {
	st := &slowStore{files: map[string]string{}, delay: 300 * time.Millisecond}
	m, _, clock := onClock(t, st)

	writeFact(t, m, "alpha is first", "subject")
	clock.advance(3 * time.Minute) // and the store has still not ingested it
	writeFact(t, m, "bravo is second", "subject")

	got := st.final("facts/projects/subject.md")
	for _, f := range []string{"alpha is first", "bravo is second"} {
		if !strings.Contains(got, f) {
			t.Errorf("%q was lost:\n%s", f, got)
		}
	}
}

// Reads are not monotonic: one reader showing the write does not mean the
// next one will. A write the store has been seen to hold is still trusted
// while the other readers catch up.
func TestAWriteSeenOnceIsKeptWhileOtherReadersCatchUp(t *testing.T) {
	st := &slowStore{files: map[string]string{}, delay: 10 * time.Millisecond, lag: 500 * time.Millisecond}
	m, g, clock := onClock(t, st)
	path := "facts/projects/subject.md"

	writeFact(t, m, "alpha is first", "subject")
	st.final(path)
	// The one reader that has it answers this.
	if cur, err := g.current(context.Background(), path); err != nil || !strings.Contains(cur.Content, "alpha") {
		t.Fatalf("current = %v, %v", cur, err)
	}
	clock.advance(3 * time.Minute)
	// The next read is served by a reader still without it.
	writeFact(t, m, "bravo is second", "subject")

	got := st.final(path)
	for _, f := range []string{"alpha is first", "bravo is second"} {
		if !strings.Contains(got, f) {
			t.Errorf("%q was lost:\n%s", f, got)
		}
	}
}

// Once the store has shown the write and its readers have had time to catch
// up, the store's copy is trusted again: a change made to the document since
// — a repair, say — is not undone by KARMAX's old copy.
func TestOnceTheStoreHasCaughtUpItsCopyIsTrustedAgain(t *testing.T) {
	st := &slowStore{files: map[string]string{}, delay: 10 * time.Millisecond}
	m, g, clock := onClock(t, st)
	path := "facts/projects/subject.md"

	writeFact(t, m, "alpha is first", "subject")
	writeFact(t, m, "bravo is a duplicate of alpha", "subject")
	st.final(path)
	if _, err := g.current(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	clock.advance(10 * time.Minute)
	st.mu.Lock()
	st.files[path] = "alpha is first"
	st.mu.Unlock()

	writeFact(t, m, "charlie is third", "subject")
	got := st.final(path)
	if strings.Contains(got, "bravo") {
		t.Errorf("a section repaired out of the document came back:\n%s", got)
	}
	for _, f := range []string{"alpha is first", "charlie is third"} {
		if !strings.Contains(got, f) {
			t.Errorf("%q was lost:\n%s", f, got)
		}
	}
}

// What KARMAX wrote is not kept for ever: a write the store never shows is
// trusted for a long while, and then no longer.
func TestAWriteIsTrustedForABoundedTime(t *testing.T) {
	st := &slowStore{files: map[string]string{}, delay: 10 * time.Millisecond}
	m, g, clock := onClock(t, st)
	path := "facts/projects/subject.md"

	writeFact(t, m, "alpha is first", "subject")
	st.final(path)
	clock.advance(30 * time.Minute)
	if _, ok := g.pendingFor(path); !ok {
		t.Fatal("a write not yet seen in the store was dropped after 30 minutes")
	}
	clock.advance(2 * time.Hour)
	if _, ok := g.pendingFor(path); ok {
		t.Error("a write was still trusted over the store two and a half hours on")
	}
}

// A forget is kept until the store shows it too. A reader still serving the
// document from before the forget, read past the old two minutes, carried
// the forgotten fact back with the next write.
func TestAForgetIsKeptUntilTheStoreShowsIt(t *testing.T) {
	const lag = 400 * time.Millisecond
	st := &slowStore{files: map[string]string{}, delay: 20 * time.Millisecond, lag: lag}
	m, _, clock := onClock(t, st)
	settle := func() { time.Sleep(lag + 100*time.Millisecond) }

	// One fact of several.
	writeFact(t, m, "alpha is first", "subject")
	writeFact(t, m, "bravo met Quinn at 19:47 IST", "subject")
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
	clock.advance(3 * time.Minute)
	writeFact(t, m, "charlie is third", "subject")
	if got := st.final("facts/projects/subject.md"); strings.Contains(got, "bravo") || !strings.Contains(got, "charlie") || !strings.Contains(got, "alpha") {
		t.Errorf("after forgetting a section and writing again three minutes on:\n%s", got)
	}

	// A whole document.
	writeFact(t, m, "delta is alone", "other")
	st.final("facts/projects/other.md")
	settle()
	if err := m.Forget("facts/projects/other.md"); err != nil {
		t.Fatalf("Forget document: %v", err)
	}
	clock.advance(3 * time.Minute)
	writeFact(t, m, "echo is new", "other")
	if got := st.final("facts/projects/other.md"); strings.Contains(got, "delta") || !strings.Contains(got, "echo") {
		t.Errorf("after forgetting a document and writing again three minutes on:\n%s", got)
	}
}
