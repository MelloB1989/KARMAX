package memory

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// The graph comes from the store, not from an LLM pass over local rows.
func TestGraphIsReadFromTheStore(t *testing.T) {
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/v1/graph") {
			return false
		}
		writeJSON(w, map[string]any{
			"nodes": []any{
				// A directory: structure, not a memory. It must not become a node.
				map[string]any{"path": "facts", "kind": "dir", "tier": "facts"},
				map[string]any{"path": "facts/people/siva.md", "kind": "file", "tier": "facts", "title": "Siva"},
				map[string]any{"path": "facts/projects/campx.md", "kind": "file", "tier": "facts"},
			},
			"edges": []any{
				map[string]any{"src": "facts/people/siva.md", "dst": "facts/projects/campx.md", "label": "works on"},
				// An edge into a node that is not present would render as a link
				// to nothing.
				map[string]any{"src": "facts/people/siva.md", "dst": "facts/gone.md", "label": "mentions"},
			},
		})
		return true
	})

	m, _ := managerWithGitLoom(t, srv.URL)
	g, err := m.Graph(context.Background(), 100)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Nodes) != 2 {
		t.Errorf("got %d nodes, want 2 (the directory must not be one)", len(g.Nodes))
	}
	if len(g.Links) != 1 {
		t.Fatalf("got %d links, want 1 (the dangling edge must be dropped)", len(g.Links))
	}
	if g.Links[0].Relation != "works on" {
		t.Errorf("relation = %q, want %q", g.Links[0].Relation, "works on")
	}
	// The title falls back to the filename, so a node is never blank.
	for _, n := range g.Nodes {
		if strings.TrimSpace(n.Title) == "" {
			t.Errorf("node %s has no title", n.ID)
		}
	}
}

// Without GitLoom there is no graph to read, and the caller has to know so it
// can build one itself rather than rendering an empty view.
func TestGraphReportsWhenTheStoreHasNone(t *testing.T) {
	m := localManager(t)
	if m.HasRemote() {
		t.Fatal("a plain manager should have no remote")
	}
	if _, err := m.Graph(context.Background(), 100); err != ErrNoRemote {
		t.Errorf("err = %v, want ErrNoRemote", err)
	}
}

// Survey is ONE call. It is used to pick a handful of candidates out of
// hundreds of memories, so a request per memory would make the reviewer and the
// cleanup flow cost hundreds of round trips to discard nearly all of them.
func TestSurveyCostsOneRequest(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/v1/tree") {
			return false
		}
		mu.Lock()
		calls++
		mu.Unlock()
		writeJSON(w, map[string]any{"tree": map[string]any{
			"path": "", "children": []any{
				map[string]any{"path": "facts/a.md", "summary": "a fact about A", "tier": "facts"},
				map[string]any{"path": "facts/b.md", "summary": "a fact about B", "tier": "facts"},
			},
		}})
		return true
	})

	m, _ := managerWithGitLoom(t, srv.URL)
	got, err := m.Survey(context.Background(), 50)
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Content != "a fact about A" {
		t.Errorf("content = %q; the summary should carry the text", got[0].Content)
	}
	// The id is the handle Forget and Update take, so it has to be the path.
	if got[0].ID != "facts/a.md" {
		t.Errorf("id = %q, want the path", got[0].ID)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("survey made %d requests, want 1", calls)
	}
}

// An update keeps the metadata that makes a memory findable.
//
// A GitLoom write replaces the whole file, so writing only the corrected text
// would drop the tags, cues and relationships — leaving a memory that is
// accurate and unreachable, which is a subtler way of losing it than deletion.
func TestUpdatePreservesWhatMakesAMemoryFindable(t *testing.T) {
	var mu sync.Mutex
	var written map[string]any

	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/memories"):
			writeJSON(w, map[string]any{
				"path": "facts/people/siva.md", "content": "the old text",
				"tags": []string{"campx", "people"}, "cues": []string{"Siva"},
				"related": []string{"facts/projects/campx.md"}, "confidence": 0.9,
			})
			return true
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/memories"):
			var body struct {
				Memories []map[string]any `json:"memories"`
			}
			_ = decodeJSON(r, &body)
			mu.Lock()
			if len(body.Memories) > 0 {
				written = body.Memories[0]
			}
			mu.Unlock()
			writeJSON(w, map[string]any{"written": 1})
			return true
		}
		return false
	})

	m, _ := managerWithGitLoom(t, srv.URL)
	if err := m.Update(context.Background(), "facts/people/siva.md", "the corrected text"); err != nil {
		t.Fatalf("update: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if written == nil {
		t.Fatal("nothing was written")
	}
	if got, _ := written["content"].(string); got != "the corrected text" {
		t.Errorf("content = %q, want the correction", got)
	}
	for _, key := range []string{"tags", "cues", "related"} {
		if v, ok := written[key]; !ok || v == nil {
			t.Errorf("%s was dropped by the update", key)
		}
	}
}

// Forget takes the handle recall hands out. Passing a local row id to a store
// that deals in paths would report success and delete nothing.
func TestForgetRefusesAHandleTheStoreCannotUse(t *testing.T) {
	srv := fakeGitLoom(t, func(http.ResponseWriter, *http.Request) bool { return false })
	m, _ := managerWithGitLoom(t, srv.URL)

	err := m.Forget("32f1bed0-7a7a-4e5f-bd5f-21271d56d7d3")
	if err == nil {
		t.Fatal("a row id was accepted as a GitLoom handle")
	}
	if !strings.Contains(err.Error(), ".md") {
		t.Errorf("the error does not say what a handle looks like: %v", err)
	}
}

// A staleness answer must not erase a whole subject.
//
// A GitLoom path names a SUBJECT and every fact about it folds in as a ##
// section — one file on the live instance holds 166. The operator is asked
// "that June deadline, still open?" and answers "done"; reading that as
// permission to delete everything KARMAX knows about the subject would destroy
// 165 unrelated facts, silently, and they would never know what went.
func TestForgettingRefusesToTakeAWholeSubjectWithIt(t *testing.T) {
	var mu sync.Mutex
	deleted := false

	body := "# Subject\n\n## first fact\nsomething\n\n## second fact\nsomething else\n"
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/memories"):
			writeJSON(w, map[string]any{"path": "facts/context/subject.md", "content": body})
			return true
		case r.Method == http.MethodDelete || strings.Contains(r.URL.Path, "forget"):
			mu.Lock()
			deleted = true
			mu.Unlock()
			writeJSON(w, map[string]any{"forgotten": 1})
			return true
		}
		return false
	})

	m, _ := managerWithGitLoom(t, srv.URL)
	removed, err := m.ForgetFact(context.Background(), "facts/context/subject.md")
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if removed {
		t.Error("a multi-fact subject reported itself deleted")
	}
	mu.Lock()
	defer mu.Unlock()
	if deleted {
		t.Error("a subject holding several facts was deleted to retire one of them")
	}
}

// A memory that holds ONE fact is exactly what the question was about, so it
// does go — otherwise nothing could ever be forgotten.
func TestForgettingASingleFactStillWorks(t *testing.T) {
	var mu sync.Mutex
	deleted := false

	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/memories"):
			writeJSON(w, map[string]any{"path": "facts/context/one.md", "content": "a single fact, no sections"})
			return true
		default:
			mu.Lock()
			deleted = true
			mu.Unlock()
			writeJSON(w, map[string]any{"forgotten": 1})
			return true
		}
	})

	m, _ := managerWithGitLoom(t, srv.URL)
	removed, err := m.ForgetFact(context.Background(), "facts/context/one.md")
	if err != nil || !removed {
		t.Fatalf("removed = %v, err = %v; a single fact should be forgettable", removed, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleted {
		t.Error("nothing was deleted")
	}
}

// Correcting one detail must not overwrite the subject it lives in.
func TestCorrectingAddsToASubjectRatherThanReplacingIt(t *testing.T) {
	var mu sync.Mutex
	var written string

	body := "# Subject\n\n## first fact\nthe old detail\n\n## second fact\nsomething unrelated\n"
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/memories"):
			writeJSON(w, map[string]any{"path": "facts/context/subject.md", "content": body})
			return true
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/memories"):
			var in struct {
				Memories []map[string]any `json:"memories"`
			}
			_ = decodeJSON(r, &in)
			mu.Lock()
			if len(in.Memories) > 0 {
				written, _ = in.Memories[0]["content"].(string)
			}
			mu.Unlock()
			writeJSON(w, map[string]any{"written": 1})
			return true
		}
		return false
	})

	m, _ := managerWithGitLoom(t, srv.URL)
	replaced, err := m.Correct(context.Background(), "facts/context/subject.md", "the corrected detail")
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if replaced {
		t.Error("a multi-fact subject was replaced wholesale by one correction")
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(written, "the corrected detail") {
		t.Error("the correction was not recorded")
	}
	if !strings.Contains(written, "something unrelated") {
		t.Error("correcting one detail destroyed the other facts about the subject")
	}
}

// Sections are how KARMAX writes facts, so counting them counts the facts.
func TestCountFactsTreatsProseAsOneFact(t *testing.T) {
	if n := CountFacts("just a sentence, no headers"); n != 1 {
		t.Errorf("prose counted as %d facts, want 1", n)
	}
	if n := CountFacts("# Title\n\n## a\nx\n\n## b\ny\n"); n != 2 {
		t.Errorf("two sections counted as %d facts, want 2", n)
	}
}
