package memory

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A write to a namespace that does not exist is a 404, and member namespaces
// are made on demand, so the first write has to create it — once.
func TestNamespaceIsCreatedOnceBeforeTheFirstWrite(t *testing.T) {
	var created, posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/namespaces":
			created.Add(1)
			writeJSON(w, map[string]any{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/memories":
			posts.Add(1)
			writeJSON(w, map[string]any{"written": 1})
		default:
			http.Error(w, `{"error":{"code":"not_found","message":"no memory"}}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	m, _ := managerWithGitLoom(t, srv.URL)
	for i := 0; i < 3; i++ {
		if err := m.Write(MemoryEntry{Role: "user", Content: "fact number " + string(rune('a'+i)), Category: "facts", Tags: []string{"s"}}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if created.Load() != 1 {
		t.Errorf("namespace created %d times, want once", created.Load())
	}
	if posts.Load() != 3 {
		t.Errorf("%d writes reached the store, want 3", posts.Load())
	}
}

// A missing namespace is not "no memory at that path", and neither is a reason
// to mark the store down when reading.
func TestMissingNamespaceIsNotAMissingMemory(t *testing.T) {
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"error": map[string]any{"code": "namespace_not_found", "message": "create it"}})
		return true
	})
	m, _ := managerWithGitLoom(t, srv.URL)

	res, err := m.Search("anything", 5)
	if err != nil || len(res) != 0 {
		t.Fatalf("search = %v, %v; want empty and no error", res, err)
	}
	if _, healthy, _ := m.RemoteStatus(); !healthy {
		t.Error("an empty namespace marked the store unhealthy")
	}
	if e, err := m.Load(context.Background(), "facts/x.md"); err != nil || e != nil {
		t.Errorf("load = %v, %v; want nil, nil", e, err)
	}
}

// Get returns the caller's tags merged with ones the server inferred. Writing
// that merge back as the caller's would harden every inferred tag.
func TestWriteFoldsOnlyTheCallersTagsAndDatesTheFile(t *testing.T) {
	var mu sync.Mutex
	var posted map[string]any

	earlier := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/memories"):
			writeJSON(w, map[string]any{
				"path": "facts/projects/s.md", "content": "## 2026-09-01 — first\n\nfirst fact",
				"tags":      []string{"s", "projects", "inferred-by-server"},
				"user_tags": []string{"s", "projects"}, "occurred_at": earlier.Unix(),
			})
			return true
		case r.Method == http.MethodPost && r.URL.Path == "/v1/memories":
			var body map[string]any
			_ = decodeJSON(r, &body)
			mu.Lock()
			posted = body
			mu.Unlock()
			writeJSON(w, map[string]any{"written": 1})
			return true
		}
		return false
	})
	m, _ := managerWithGitLoom(t, srv.URL)

	said := time.Date(2026, 9, 30, 21, 0, 0, 0, time.UTC)
	if err := m.Write(MemoryEntry{
		Role: "user", Content: "second fact", Category: "project", Tags: []string{"s", "person:ab"},
		OccurredAt: said,
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	mems, _ := posted["memories"].([]any)
	if len(mems) != 1 {
		t.Fatalf("posted %v", posted)
	}
	mem := mems[0].(map[string]any)
	tags := strings.Join(toStrings(mem["tags"]), ",")
	if strings.Contains(tags, "inferred-by-server") {
		t.Errorf("an inferred tag was written back as the caller's: %s", tags)
	}
	if !strings.Contains(tags, "person:ab") {
		t.Errorf("the new tag was lost: %s", tags)
	}
	if got, _ := mem["occurred_at"].(float64); int64(got) != said.Unix() {
		t.Errorf("occurred_at = %v, want the event time %d, not the write time", mem["occurred_at"], said.Unix())
	}
	if !strings.Contains(mem["content"].(string), "## 2026-09-30 — second fact") {
		t.Errorf("the section is not dated by when it happened:\n%v", mem["content"])
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// Retrieval asks for the fused lane ranking, bounds the content it gets back,
// and passes the time range the caller gave.
func TestSearchAsksForFusedRankingWithTheRange(t *testing.T) {
	var mu sync.Mutex
	var q map[string][]string
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/v1/retrieve") {
			return false
		}
		mu.Lock()
		q = r.URL.Query()
		mu.Unlock()
		writeJSON(w, map[string]any{"memories": []any{}, "rank": "fused"})
		return true
	})
	m, _ := managerWithGitLoom(t, srv.URL)

	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	if _, err := m.SearchWith("what did she say", 7, SearchOpts{Since: since, Until: until, TagsAll: []string{"chat:1"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for key, want := range map[string]string{
		"rank": "fused", "time_field": "occurred", "tags_all": "chat:1", "limit": "7",
		"since": since.Format(time.RFC3339), "until": until.Format(time.RFC3339),
	} {
		if got := q[key]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want %s", key, got, want)
		}
	}
	if q["max_chars"] == nil {
		t.Error("no content budget was sent; one hit can be a whole 100KB file")
	}
}

// v0.6.0 refuses an empty question client-side. That is nothing to look up, not
// a store that is down.
func TestAnEmptyQuestionDoesNotMarkTheStoreDown(t *testing.T) {
	var hit atomic.Bool
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		hit.Store(true)
		return false
	})
	m, _ := managerWithGitLoom(t, srv.URL)
	res, err := m.Search("  ", 5)
	if err != nil || len(res) != 0 {
		t.Fatalf("search = %v, %v", res, err)
	}
	if hit.Load() {
		t.Error("an empty question made a request")
	}
	if _, healthy, _ := m.RemoteStatus(); !healthy {
		t.Error("an empty question marked the store unhealthy")
	}
}

// Recency is the store's, not the tree's: a listing with no question, newest
// first, handed back newest last.
func TestRecentIsTheStoresNewestFirstListingReversed(t *testing.T) {
	var treeCalls atomic.Int32
	var q map[string][]string
	var mu sync.Mutex
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/tree"):
			treeCalls.Add(1)
			return false
		case strings.HasPrefix(r.URL.Path, "/v1/retrieve"):
			mu.Lock()
			q = r.URL.Query()
			mu.Unlock()
			writeJSON(w, map[string]any{"memories": []any{
				map[string]any{"path": "facts/new.md", "content": "newest", "updated_at": 300},
				map[string]any{"path": "facts/mid.md", "content": "middle", "updated_at": 200},
				map[string]any{"path": "facts/old.md", "content": "oldest", "updated_at": 100},
			}})
			return true
		}
		return false
	})
	m, _ := managerWithGitLoom(t, srv.URL)

	got, err := m.Recent(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "facts/old.md" || got[2].ID != "facts/new.md" {
		t.Fatalf("order = %v, want oldest first, newest last", got)
	}
	if got[2].CreatedAt.Unix() != 300 {
		t.Errorf("update time lost: %v", got[2].CreatedAt)
	}
	if treeCalls.Load() != 0 {
		t.Error("recent walked the tree")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(q["tiers"]) == 0 || q["time_field"][0] != "updated" {
		t.Errorf("listing params = %v", q)
	}
	if _, has := q["q"]; has && q["q"][0] != "" {
		t.Errorf("a question was sent: %v", q["q"])
	}
}

func TestOlderBoundsByUpdateTime(t *testing.T) {
	var until string
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/v1/retrieve") {
			return false
		}
		until = r.URL.Query().Get("until")
		writeJSON(w, map[string]any{"memories": []any{}})
		return true
	})
	m, _ := managerWithGitLoom(t, srv.URL)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := m.Older(context.Background(), cutoff, 50); err != nil {
		t.Fatal(err)
	}
	if until != cutoff.Format(time.RFC3339) {
		t.Errorf("until = %q", until)
	}
}

func TestOnly404WithoutANamespaceCodeIsAMissingMemory(t *testing.T) {
	if isNotFound(nil) || isNamespaceMissing(nil) {
		t.Fatal("nil is neither")
	}
}

func TestParseWhen(t *testing.T) {
	loc := time.FixedZone("z", 3600)
	if got, ok := ParseWhen("2026-03-02", loc, false); !ok || !got.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, loc)) {
		t.Errorf("start of day = %v %v", got, ok)
	}
	got, _ := ParseWhen("2026-03-02", loc, true)
	if got.Day() != 2 || got.Hour() != 23 {
		t.Errorf("an until date must include its whole day, got %v", got)
	}
	if _, ok := ParseWhen("last tuesday", loc, false); ok {
		t.Error("prose parsed as a time")
	}
}

// slowStore is a backend whose writes land after a delay, as the real one's do.
type slowStore struct {
	mu    sync.Mutex
	files map[string]string
	delay time.Duration
	wg    sync.WaitGroup
	// prev chains applications so they land in arrival order, as a per-namespace
	// write queue does.
	prev chan struct{}
	// lag, when set, makes reads disagree as the store's retrieve workers do:
	// after a change ONE read sees it, and the rest serve a snapshot that old.
	lag       time.Duration
	snaps     []storeSnap
	freshNext bool
	// posts counts the writes that reached the store.
	posts atomic.Int32
	// meta is each document's frontmatter, kept as the store keeps it.
	meta map[string]storeMeta
}

// storeMeta is the frontmatter a write sets and a read returns.
type storeMeta struct {
	Tags       []string `json:"tags"`
	Cues       []string `json:"cues"`
	Related    []string `json:"related"`
	Confidence float64  `json:"confidence"`
	OccurredAt int64    `json:"occurred_at"`
	TTL        string   `json:"ttl"`
}

type storeSnap struct {
	at    time.Time
	files map[string]string
}

// snap records the store's state after a change; called with mu held.
func (s *slowStore) snap() {
	if s.lag == 0 {
		return
	}
	c := make(map[string]string, len(s.files))
	for k, v := range s.files {
		c[k] = v
	}
	s.snaps = append(s.snaps, storeSnap{at: time.Now(), files: c})
	s.freshNext = true
}

// view is what a read sees; called with mu held.
func (s *slowStore) view(path string) map[string]string {
	if s.lag == 0 || s.freshNext {
		s.freshNext = false
		return s.files
	}
	cut := time.Now().Add(-s.lag)
	v := map[string]string{}
	for _, sn := range s.snaps {
		if sn.at.After(cut) {
			break
		}
		v = sn.files
	}
	return v
}

func (s *slowStore) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/namespaces":
		writeJSON(w, map[string]any{"status": "ok"})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/memories":
		var body struct {
			Memories []struct {
				Path, Content string
				storeMeta
			} `json:"memories"`
		}
		_ = decodeJSON(r, &body)
		s.posts.Add(1)
		s.wg.Add(1)
		s.mu.Lock()
		before, mine := s.prev, make(chan struct{})
		s.prev = mine
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer close(mine)
			if before != nil {
				<-before
			}
			time.Sleep(s.delay)
			s.mu.Lock()
			for _, m := range body.Memories {
				s.files[m.Path] = m.Content
				if s.meta == nil {
					s.meta = map[string]storeMeta{}
				}
				s.meta[m.Path] = m.storeMeta
			}
			s.snap()
			s.mu.Unlock()
		}()
		writeJSON(w, map[string]any{"written": 1})
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/memories":
		paths := strings.Split(r.URL.Query().Get("path"), ",")
		s.wg.Add(1)
		s.mu.Lock()
		before, mine := s.prev, make(chan struct{})
		s.prev = mine
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer close(mine)
			if before != nil {
				<-before
			}
			time.Sleep(s.delay)
			s.mu.Lock()
			for _, p := range paths {
				file, slug, isSection := strings.Cut(p, "#")
				if !isSection {
					delete(s.files, file)
					delete(s.meta, file)
					continue
				}
				if rest := cutSection(s.files[file], slug); strings.TrimSpace(rest) == "" {
					delete(s.files, file)
					delete(s.meta, file)
				} else {
					s.files[file] = rest
				}
			}
			s.snap()
			s.mu.Unlock()
		}()
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{"status": "accepted"})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/memories"):
		p := r.URL.Query().Get("path")
		file, slug, isSection := strings.Cut(p, "#")
		s.mu.Lock()
		c, ok := s.view(p)[file]
		meta := s.meta[file]
		s.mu.Unlock()
		title := ""
		if ok && isSection {
			ok = cutSection(c, slug) != c
			for _, l := range strings.Split(c, "\n") {
				if strings.HasPrefix(l, "## ") && serverSlug(strings.TrimPrefix(l, "## ")) == slug {
					title = strings.TrimPrefix(l, "## ")
				}
			}
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"error": map[string]any{"code": "not_found", "message": "no memory"}})
			return
		}
		out := map[string]any{"path": p, "title": title, "content": c, "updated_at": time.Now().Unix() - 3600,
			"user_tags": meta.Tags, "cues": meta.Cues, "related": meta.Related, "confidence": meta.Confidence}
		if meta.OccurredAt != 0 {
			out["occurred_at"] = meta.OccurredAt
		}
		writeJSON(w, out)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/retrieve"):
		// Every document holding the word asked for, and each of its sections
		// that does, as one reader sees them. No question lists everything.
		q := strings.ToLower(r.URL.Query().Get("q"))
		has := func(text string) bool { return q == "" || strings.Contains(strings.ToLower(text), q) }
		s.mu.Lock()
		view := s.view("")
		var hits []any
		for p, doc := range view {
			if !has(doc) {
				continue
			}
			hits = append(hits, map[string]any{"path": p, "content": doc, "score": 0.5})
			// A section's hit carries its header, which opens with its fact.
			for _, h := range sectionHeaders(doc) {
				if has(h) {
					hits = append(hits, map[string]any{"path": p + "#" + serverSlug(h), "title": h, "content": "## " + h, "score": 0.6})
				}
			}
		}
		s.mu.Unlock()
		writeJSON(w, map[string]any{"memories": hits, "rank": "fused"})
	default:
		http.NotFound(w, r)
	}
}

// serverSlug is how the store addresses a header: punctuation dropped, spaces
// to single hyphens — not how KARMAX would slug it.
func serverSlug(header string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(header)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			hyphen = false
		case r == ' ' || r == '-' || r == '_':
			if !hyphen && b.Len() > 0 {
				b.WriteByte('-')
				hyphen = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// cutSection is the store's forget of one "## " section; doc is returned
// unchanged when no header has that slug.
func cutSection(doc, slug string) string {
	lines := strings.Split(doc, "\n")
	start, end := -1, len(lines)
	for i, l := range lines {
		if !strings.HasPrefix(l, "## ") {
			continue
		}
		if start >= 0 {
			end = i
			break
		}
		if serverSlug(strings.TrimPrefix(l, "## ")) == slug {
			start = i
		}
	}
	if start < 0 {
		return doc
	}
	return strings.TrimSpace(strings.Join(append(lines[:start:start], lines[end:]...), "\n"))
}

func (s *slowStore) final(path string) string {
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[path]
}

// A write whose read-before-write cannot yet see the previous one must still
// carry it, or the first fact about a subject is silently replaced.
func TestQuickWritesToOneSubjectLoseNothingOnASlowStore(t *testing.T) {
	st := &slowStore{files: map[string]string{}, delay: 150 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)

	facts := []string{"alpha is first", "bravo is second", "charlie is third", "delta is fourth"}
	for _, f := range facts {
		if err := m.Write(MemoryEntry{Role: "user", Content: f, Category: "project", Tags: []string{"subject"}}); err != nil {
			t.Fatal(err)
		}
	}
	got := st.final("facts/projects/subject.md")
	for _, f := range facts {
		if !strings.Contains(got, f) {
			t.Errorf("%q was lost:\n%s", f, got)
		}
	}
}

// The same from many goroutines at once, through the backend directly (the
// manager's own lock would hide it). Run with -race.
func TestConcurrentWritesToOnePathLoseNothingOnASlowStore(t *testing.T) {
	st := &slowStore{files: map[string]string{}, delay: 80 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)
	m.mu.Lock()
	g := m.remote
	m.mu.Unlock()

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := MemoryEntry{Content: fmt.Sprintf("fact-%02d about the subject", i), Category: "project", Tags: []string{"subject"}}
			if err := g.write(context.Background(), e, "facts/projects/subject.md", nil); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	got := st.final("facts/projects/subject.md")
	for i := 0; i < n; i++ {
		if f := fmt.Sprintf("fact-%02d", i); !strings.Contains(got, f) {
			t.Errorf("%s was lost", f)
		}
	}
}

// Once the server holds what was written, its copy is trusted again.
func TestServerCopyWinsOnceItHasCaughtUp(t *testing.T) {
	st := &slowStore{files: map[string]string{}, delay: time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	t.Cleanup(srv.Close)
	m, _ := managerWithGitLoom(t, srv.URL)
	m.mu.Lock()
	g := m.remote
	m.mu.Unlock()

	if err := m.Write(MemoryEntry{Content: "first fact here", Category: "project", Tags: []string{"s"}}); err != nil {
		t.Fatal(err)
	}
	st.final("facts/projects/s.md")
	// Another writer changes the file after KARMAX's write landed.
	st.mu.Lock()
	st.files["facts/projects/s.md"] += "\n\n## other writer\n\nsomething else"
	st.mu.Unlock()
	cur, err := g.current(context.Background(), "facts/projects/s.md")
	if err != nil || !strings.Contains(cur.Content, "other writer") {
		t.Fatalf("current = %v, %v; want the server's newer copy", cur, err)
	}
}
