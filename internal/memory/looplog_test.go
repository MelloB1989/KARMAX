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

// A run's lines are one entry in the loop's log, dated by when the run began.
func TestARunsLinesAreOneEntryInTheLoopsLog(t *testing.T) {
	began := time.Date(2026, 10, 31, 23, 58, 0, 0, time.UTC)
	e, ok := LoopRunEntry("night-watch", []string{"group-a [waiting]: Omar owes the deck", "group-b [flagged]: Priya asked for the invoice"}, began)
	if !ok {
		t.Fatal("a run with lines made no entry")
	}
	if !IsLoopLog(e) || PathFor(e, nil) != "facts/context/night-watch/2026-10.md" {
		t.Errorf("filed as %q at %s", e.Category, PathFor(e, nil))
	}
	if e.Content != "group-a [waiting]: Omar owes the deck\ngroup-b [flagged]: Priya asked for the invoice" {
		t.Errorf("content = %q", e.Content)
	}
	if _, ok := LoopRunEntry("night-watch", nil, began); ok {
		t.Error("a run that said nothing made an entry")
	}
}

// What a line says, without when it was said: the run's own date and clock
// time are not news, any other date or time is.
func TestALinesKeyIgnoresOnlyTheRunsOwnStamp(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	run1 := time.Date(2026, 10, 31, 2, 10, 0, 0, ist)
	run2 := time.Date(2026, 10, 31, 5, 10, 30, 0, ist)
	next := time.Date(2026, 11, 1, 9, 0, 0, 0, ist)
	key := func(line string, at time.Time) string { return LogLineKey(line, at, at.Add(time.Minute), ist) }

	same := [][2]string{
		// The loop's own stamp, in the forms loops write it.
		{"(night-watch, 31 Oct 02:10) group-a [waiting]: Omar owes the deck", "(night-watch, 31 Oct 05:10) group-a [waiting]: Omar owes the deck"},
		{"2026-10-31T02:10:00 group-a quiet", "2026-10-31T05:10:30 group-a quiet"},
		{"Oct 31, 2:10 AM: relay ok", "Oct 31, 5:11 am: relay ok"},
		// A WASM loop's clock is UTC.
		{"(night-watch, 30 Oct 20:40) group-a quiet", "(night-watch, 30 Oct 23:40) group-a quiet"},
	}
	for _, p := range same {
		if a, b := key(p[0], run1), key(p[1], run2); a != b {
			t.Errorf("only the run's stamp differs, yet\n%q\n%q", a, b)
		}
	}
	differ := [][2]string{
		{"group-a [waiting]: Omar owes the deck", "group-a [flagged]: Omar owes the deck"},
		// A date or time that is not the run's own is what the line says.
		{"(night-watch, 31 Oct 02:10) the call moved to 4 Nov", "(night-watch, 31 Oct 05:10) the call moved to 5 Nov"},
		{"(night-watch, 31 Oct 02:10) the call is at 18:30", "(night-watch, 31 Oct 05:10) the call is at 19:30"},
		{"relay answered in 102 ms", "relay answered in 140 ms"},
	}
	for _, p := range differ {
		if a, b := key(p[0], run1), key(p[1], run2); a == b {
			t.Errorf("these say different things, yet both key to %q", a)
		}
	}
	// A new day's run stamps a new date, and is still the same line.
	if a, b := key("(night-watch, 31 Oct 05:10) group-a quiet", run2), key("(night-watch, 1 Nov 09:00) group-a quiet", next); a != b {
		t.Errorf("a stamp from the next day changed the key:\n%q\n%q", a, b)
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
