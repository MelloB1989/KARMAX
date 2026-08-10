package memory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// GitLoom is the store, not a layer over one.
//
// The mirror it replaced kept every memory in SQLite as well, which meant two
// stores that could disagree — and the disagreement was silent, because a
// memory forgotten in one and recalled from the other is indistinguishable
// from a memory that was never forgotten. If a write starts landing in both
// again, that failure mode comes back, so this pins it directly.
func TestGitLoomWritesDoNotTouchSQLite(t *testing.T) {
	var wrote bool
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/memories") {
			wrote = true
			writeJSON(w, map[string]any{"written": 1})
			return true
		}
		return false
	})

	m, db := managerWithGitLoom(t, srv.URL)
	if err := m.Write(MemoryEntry{
		Role: "user", Content: "CampX pays in three instalments", Category: "facts",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !wrote {
		t.Fatal("the memory never reached GitLoom")
	}

	n, err := db.CountMemoryEntries("test-ns")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d memories were mirrored into SQLite; GitLoom is the store", n)
	}
}

// A write that GitLoom refuses is a failed write. The outbox used to swallow
// it and retry later, which was right when GitLoom was across home internet —
// but a caller told "remembered" when nothing was is the worse failure now
// that it runs locally.
func TestAFailedGitLoomWriteIsReportedToTheCaller(t *testing.T) {
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost {
			http.Error(w, `{"error":"disk full"}`, http.StatusInternalServerError)
			return true
		}
		return false
	})

	m, _ := managerWithGitLoom(t, srv.URL)
	if err := m.Write(MemoryEntry{Role: "user", Content: "something worth keeping"}); err == nil {
		t.Fatal("a refused write reported success")
	}
}

// Folding is what keeps a whole-file write from deleting history: every memory
// about one subject shares a path, so the newest fact must carry the older
// ones with it. A read that fails must refuse rather than overwrite.
func TestAWriteRefusesRatherThanOverwriteUnreadableHistory(t *testing.T) {
	srv := fakeGitLoom(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/memories") {
			// Exists, but reads back empty — the shape that silently destroyed
			// section-formatted files under an older API.
			writeJSON(w, map[string]any{"path": "facts/campx.md", "content": ""})
			return true
		}
		if r.Method == http.MethodPost {
			t.Error("a write proceeded despite unreadable history")
			writeJSON(w, map[string]any{"written": 1})
			return true
		}
		return false
	})

	m, _ := managerWithGitLoom(t, srv.URL)
	err := m.Write(MemoryEntry{Role: "user", Content: "a new fact about CampX", Category: "facts"})
	if err == nil {
		t.Fatal("the write was accepted without preserving what was stored")
	}
}

// With no GitLoom configured, SQLite is still the whole memory — the
// self-hosted path, which the move must not have quietly broken.
func TestWithoutGitLoomSQLiteIsStillTheStore(t *testing.T) {
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "k.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	m := NewManager("test", "test-ns", dir, db, zap.NewNop())
	if err := m.Write(MemoryEntry{Role: "user", Content: "a local-only fact"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	n, err := db.CountMemoryEntries("test-ns")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("stored %d memories locally, want 1", n)
	}
}

func managerWithGitLoom(t *testing.T, baseURL string) (*Manager, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "k.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	m := NewManager("test", "test-ns", dir, db, zap.NewNop())
	m.UseGitLoom(GitLoomConfig{APIKey: "test-key", BaseURL: baseURL, Namespace: "test-ns"})
	t.Cleanup(m.Stop)
	return m, db
}

// fakeGitLoom serves what handle answers and 404s the rest, which is the
// "no memory at that path" the first write of a subject expects.
func fakeGitLoom(t *testing.T, handle func(http.ResponseWriter, *http.Request) bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handle(w, r) {
			return
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// localManager builds a manager with no GitLoom, which is the offline path.
func localManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "k.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m := NewManager("test", "test-ns", dir, db, zap.NewNop())
	t.Cleanup(m.Stop)
	return m
}

// decodeJSON reads a request body in a test handler.
func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}
