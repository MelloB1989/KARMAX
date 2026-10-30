package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// A correction differs from what it corrects by a word or two, so a check for
// "similar memory already exists" read every correction as a duplicate and
// dropped it — reporting success, so the agent believed it was saved.
func TestACorrectionIsNotDroppedAsADuplicate(t *testing.T) {
	tool, sc, _ := ingestTool(t)
	ctx := context.Background()

	ingest(t, tool, ctx, "Omar Haddad, who runs delivery for the Contoso account, uses the work email omar@contoso.example", nil)
	res := ingest(t, tool, ctx, "Omar Haddad, who runs delivery for the Contoso account, uses the work email omar@fabrikam.example", nil)

	out := fmt.Sprint(res.Output)
	if strings.Contains(strings.ToLower(out), "skipping") {
		t.Fatalf("the correction was dropped: %s", out)
	}
	hits, _ := sc.Global().SearchSemantic("fabrikam", 10)
	found := false
	for _, h := range hits {
		found = found || strings.Contains(h.Entry.Content, "fabrikam")
	}
	if !found {
		t.Error("the corrected fact is not in memory")
	}
	if !strings.Contains(out, "memory.forget") {
		t.Errorf("the agent was not told the old fact is still there: %s", out)
	}
}

func memoryWithGitLoom(t *testing.T, h http.HandlerFunc) *memory.Manager {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	db, err := store.New(filepath.Join(t.TempDir(), "m.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m := memory.NewManager("nexus", "ns", t.TempDir(), db, zap.NewNop())
	t.Cleanup(m.Stop)
	m.UseGitLoom(memory.GitLoomConfig{APIKey: "k", BaseURL: srv.URL, Namespace: "ns"})
	return m
}

// Forgetting one wrong fact must not take every other fact about its subject.
// A document is one subject; its sections are the facts.
func TestForgetRefusesADocumentOfManyFacts(t *testing.T) {
	var mu sync.Mutex
	deleted := false
	m := memoryWithGitLoom(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"path": "facts/projects/contoso.md",
				"content": "## 2026-08-01 — first\n\none\n\n## 2026-08-02 — second\n\ntwo"})
		case http.MethodDelete:
			mu.Lock()
			deleted = true
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		}
	})
	tool := &MemoryForgetTool{MemoryMgr: m, AgentID: "nexus"}

	res, err := tool.Execute(context.Background(), map[string]any{"id": "facts/projects/contoso.md"})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if deleted {
		t.Error("a document of two facts was deleted to forget one")
	}
	if !res.IsError {
		t.Errorf("reported success: %v", res.Output)
	}
	if !strings.Contains(fmt.Sprint(res.Output, res.Error), "#") {
		t.Errorf("the refusal does not say to forget the one fact by its section: %v %v", res.Output, res.Error)
	}
}

func TestCorrectReplacesAFact(t *testing.T) {
	_, sc, db := ingestTool(t)
	mgr := sc.Global()
	ctx := context.Background()
	if err := mgr.Write(memory.MemoryEntry{Content: "Omar Haddad works at Contoso.", Category: "relationship", Tags: []string{"omar"}}); err != nil {
		t.Fatal(err)
	}
	hits, _ := mgr.SearchSemantic("Contoso", 5)
	if len(hits) == 0 {
		t.Fatal("setup: nothing stored")
	}
	tool := &MemoryCorrectTool{Store: db, MemoryMgr: mgr, AgentID: "nexus"}
	res, err := tool.Execute(ctx, map[string]any{"id": hits[0].Entry.ID, "content": "Omar Haddad works at Fabrikam (since October 2026)."})
	if err != nil || res.IsError {
		t.Fatalf("correct: %v %v", res.Error, err)
	}
	after, _ := mgr.SearchSemantic("Omar Haddad works", 5)
	for _, h := range after {
		if strings.Contains(h.Entry.Content, "Contoso") {
			t.Errorf("the corrected fact is still there: %q", h.Entry.Content)
		}
	}
	if len(after) == 0 || !strings.Contains(after[0].Entry.Content, "Fabrikam") {
		t.Errorf("the correction is not in memory: %+v", after)
	}
}
