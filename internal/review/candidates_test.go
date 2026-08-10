package review

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// The reviewer reads the store that holds the memories.
//
// It used to read the local table, which stopped being the memory when GitLoom
// took over — so it would have gone on asking the operator about facts from
// before the cutover, forever, while never noticing anything written since.
func TestCandidatesComeFromTheMemoryStore(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)

	var mu sync.Mutex
	loaded := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/tree"):
			writeJSON(w, map[string]any{"tree": map[string]any{
				"path": "", "children": []any{
					// Time-sensitive: passes the pre-filter, gets fetched.
					map[string]any{"path": "facts/a.md", "tier": "facts",
						"summary": "Siva will get back by Friday about the deadline"},
					// Durable identity fact: must NOT cost a fetch.
					map[string]any{"path": "facts/b.md", "tier": "facts",
						"summary": "Siva is the CTO and prefers email"},
				},
			}})
		case strings.HasPrefix(r.URL.Path, "/v1/memories"):
			path := r.URL.Query().Get("path")
			mu.Lock()
			loaded[path] = true
			mu.Unlock()
			writeJSON(w, map[string]any{
				"path": path, "content": "the full text of " + path, "updated": old,
			})
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	r := reviewerWithGitLoom(t, srv.URL)
	got := r.staleMemories(context.Background(), time.Now().Add(-4*24*time.Hour))

	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].id != "facts/a.md" {
		t.Errorf("candidate id = %q, want the path so resolving it can act on it", got[0].id)
	}
	if !strings.Contains(got[0].text, "full text") {
		t.Errorf("candidate carries the summary, not the memory: %q", got[0].text)
	}

	mu.Lock()
	defer mu.Unlock()
	// The pre-filter runs on summaries precisely so the durable fact never
	// costs a request. A store of a few hundred memories would otherwise be a
	// few hundred round trips per review tick.
	if loaded["facts/b.md"] {
		t.Error("a memory that failed the text pre-filter was fetched anyway")
	}
}

// A memory that is recent is not stale, however time-sensitive it reads.
func TestRecentMemoriesAreNotCandidates(t *testing.T) {
	fresh := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/tree"):
			writeJSON(w, map[string]any{"tree": map[string]any{
				"path": "", "children": []any{
					map[string]any{"path": "facts/a.md", "tier": "facts",
						"summary": "the deadline is Friday and it is still open"},
				},
			}})
		case strings.HasPrefix(r.URL.Path, "/v1/memories"):
			writeJSON(w, map[string]any{
				"path": r.URL.Query().Get("path"), "content": "written an hour ago", "updated": fresh,
			})
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	r := reviewerWithGitLoom(t, srv.URL)
	if got := r.staleMemories(context.Background(), time.Now().Add(-4*24*time.Hour)); len(got) != 0 {
		t.Errorf("got %d candidates, want none — the memory is an hour old", len(got))
	}
}

// With no memory manager at all the reviewer degrades to reminders rather than
// panicking, since a nil manager is the no-memory install.
func TestNoMemoryManagerIsNotAFailure(t *testing.T) {
	r := &Reviewer{log: zap.NewNop()}
	if got := r.staleMemories(context.Background(), time.Now()); got != nil {
		t.Errorf("got %d candidates from a reviewer with no memory", len(got))
	}
}

func reviewerWithGitLoom(t *testing.T, baseURL string) *Reviewer {
	t.Helper()
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "k.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	mgr := memory.NewManager("test", "test-ns", dir, db, zap.NewNop())
	mgr.UseGitLoom(memory.GitLoomConfig{APIKey: "k", BaseURL: baseURL, Namespace: "test-ns"})
	t.Cleanup(mgr.Stop)

	return &Reviewer{cfg: Config{Namespace: "test-ns"}, store: db, mem: mgr, log: zap.NewNop()}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
