package memory

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// A loop files a line about every run. Filed like a fact — every line about
// one subject in one document — its record grew without end towards the
// store's request limit, past which nothing more about it can be written.

func TestALoopLogIsFiledByMonth(t *testing.T) {
	st := &slowStore{files: map[string]string{}, delay: 5 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)

	oct := LoopLog("night-watch", "(night-watch, 30 Oct 22:10) group-a [flagged]: Quinn asked about the demo")
	oct.OccurredAt = time.Date(2026, 10, 30, 22, 10, 0, 0, time.UTC)
	nov := LoopLog("night-watch", "(night-watch, 2 Nov 08:00) group-a [quiet]: nothing new")
	nov.OccurredAt = time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	for _, e := range []MemoryEntry{oct, nov} {
		if err := m.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.final("facts/context/night-watch/2026-10.md"); !strings.Contains(got, "Quinn asked about the demo") {
		t.Errorf("October's line is not in October's log: %v", st.paths())
	}
	if got := st.final("facts/context/night-watch/2026-11.md"); !strings.Contains(got, "nothing new") {
		t.Errorf("November's line is not in November's log: %v", st.paths())
	}
}

// However busy the loop, no document of its log grows past the bound: a
// month's log that is full continues in a new part, and nothing is lost.
func TestAFullLoopLogContinuesInANewPart(t *testing.T) {
	defer setLogDocCap(600)()
	st := &slowStore{files: map[string]string{}, delay: 5 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)

	day := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	var lines []string
	for i := 0; i < 24; i++ {
		line := fmt.Sprintf("(relay-check, 12 Oct %02d:00) relay %02d answered in %d ms", i, i, 100+i)
		lines = append(lines, line)
		e := LoopLog("relay-check", line)
		e.OccurredAt = day.Add(time.Duration(i) * time.Minute)
		if err := m.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	st.final("")

	st.mu.Lock()
	defer st.mu.Unlock()
	var parts []string
	for p := range st.files {
		if strings.HasPrefix(p, "facts/context/relay-check/") {
			parts = append(parts, p)
		}
	}
	sort.Strings(parts)
	if len(parts) < 3 {
		t.Fatalf("the log was not split: %v", parts)
	}
	for _, want := range []string{"facts/context/relay-check/2026-10.md", "facts/context/relay-check/2026-10-part-2.md", "facts/context/relay-check/2026-10-part-3.md"} {
		if _, ok := st.files[want]; !ok {
			t.Errorf("no %s among %v", want, parts)
		}
	}
	all := ""
	for _, p := range parts {
		doc := st.files[p]
		if len(doc) > 600 {
			t.Errorf("%s is %d bytes, past the 600-byte bound", p, len(doc))
		}
		all += doc + "\n"
	}
	for _, l := range lines {
		if n := factCount(all, l); n != 1 {
			t.Errorf("%q is held %d times across the parts, want once", l, n)
		}
	}
}

// The bound is a loop log's policy, not every document's: a subject with many
// facts stays one document.
func TestOnlyALoopLogIsSplit(t *testing.T) {
	defer setLogDocCap(200)()
	st := &slowStore{files: map[string]string{}, delay: 5 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)
	for i := 0; i < 8; i++ {
		if err := m.Write(MemoryEntry{Content: fmt.Sprintf("Contoso fact number %d is worth keeping in one place", i),
			Category: "project", Tags: []string{"contoso"}}); err != nil {
			t.Fatal(err)
		}
	}
	got := st.final("facts/projects/contoso.md")
	for i := 0; i < 8; i++ {
		if !strings.Contains(got, fmt.Sprintf("fact number %d ", i)) {
			t.Errorf("fact %d is not in the subject's document:\n%s", i, got)
		}
	}
}

func TestALoopLogIsFiledUnderItsLoopWhateverItsName(t *testing.T) {
	e := LoopLog("summary", "(summary) wrote the evening summary")
	e.OccurredAt = time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	if got := PathFor(e, nil); got != "facts/context/summary/2026-09.md" {
		t.Errorf("PathFor = %q", got)
	}
}

// A log's documents are known by their path, so readers that look for facts —
// the staleness review, the merge pass — can leave a loop's record alone.
func TestALoopLogsDocumentsAreKnownByTheirPath(t *testing.T) {
	e := LoopLog("night-watch", "(night-watch) quiet")
	e.OccurredAt = time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	base := PathFor(e, nil)
	for _, p := range []string{base, logPart(base, 2), logPart(base, 12) + "#2026-10-30-night-watch-quiet"} {
		if !IsLoopLogPath(p) {
			t.Errorf("%s is not recognised as a loop's log", p)
		}
	}
	for _, p := range []string{
		"facts/context/night-watch.md", "facts/context/2026-10-25.md", "facts/projects/contoso.md",
		"facts/projects/contoso/2026-10.md", "incidents/2026/10/contoso.md", "facts/context/night-watch/notes.md",
		"facts/context/a/b/2026-10.md",
	} {
		if IsLoopLogPath(p) {
			t.Errorf("%s is taken for a loop's log", p)
		}
	}
}

// setLogDocCap bounds loop-log documents for one test.
func setLogDocCap(n int) func() {
	was := logDocCap
	logDocCap = n
	return func() { logDocCap = was }
}

// paths lists what the store holds.
func (s *slowStore) paths() []string {
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for p := range s.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
