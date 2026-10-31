package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func reviewStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "r.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// A question expires when it goes unanswered, but an answer that arrives after
// that is still the operator's answer: it is recorded and acted on.
func TestALateAnswerToAnExpiredQuestionIsKept(t *testing.T) {
	s := reviewStore(t)
	if err := s.CreateReview(store.StoredReview{ID: "rv-1", Namespace: "ns", TargetKind: "reminder", DedupKey: "k1",
		Question: "Is the parcel still on its way?", CreatedAt: time.Now().Add(-10 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ExpireReviews("ns", time.Now().Add(-7*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("ExpireReviews = %d, %v", n, err)
	}

	tool := &ReviewResolveTool{Store: s, AgentID: "nexus", Namespace: "ns"}
	res, err := tool.Execute(context.Background(), map[string]any{"id": "rv-1", "answer": "it arrived yesterday", "resolution": "done"})
	if err != nil || res.IsError {
		t.Fatalf("Execute = %v, %v", res, err)
	}
	rev, _ := s.GetReview("rv-1")
	if rev.Answer != "it arrived yesterday" || rev.Resolution != "done" || rev.Status != "resolved" {
		t.Errorf("the late answer was not kept: status %q, resolution %q, answer %q", rev.Status, rev.Resolution, rev.Answer)
	}
}

// A question already answered is not answered again by a second reply: the
// first answer stands.
func TestAnAnsweredQuestionKeepsItsAnswer(t *testing.T) {
	s := reviewStore(t)
	if err := s.CreateReview(store.StoredReview{ID: "rv-2", Namespace: "ns", TargetKind: "reminder", DedupKey: "k2",
		Question: "Is the plumber still coming?"}); err != nil {
		t.Fatal(err)
	}
	tool := &ReviewResolveTool{Store: s, AgentID: "nexus", Namespace: "ns"}
	for _, answer := range []string{"yes, Monday", "no idea"} {
		if _, err := tool.Execute(context.Background(), map[string]any{"id": "rv-2", "answer": answer, "resolution": "kept"}); err != nil {
			t.Fatal(err)
		}
	}
	if rev, _ := s.GetReview("rv-2"); rev.Answer != "yes, Monday" {
		t.Errorf("answer = %q, want the first one", rev.Answer)
	}
}

// "Updated" from a chat reply corrects the one fact asked about. Rewriting the
// whole document with the corrected sentence erased every other fact filed
// about the subject.
func TestAnUpdatedAnswerKeepsTheSubjectsOtherFacts(t *testing.T) {
	const doc = "## 2026-08-01 — Basil is watered on Sundays\n\nBasil is watered on Sundays.\n\n## 2026-08-02 — Basil gets fed monthly\n\nBasil gets fed monthly."
	var mu sync.Mutex
	var written string
	m := memoryWithGitLoom(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"path": "facts/operator/basil.md", "content": doc,
				"updated_at": time.Now().Unix() - 3600})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/memories":
			var body struct {
				Memories []struct{ Content string } `json:"memories"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			if len(body.Memories) > 0 {
				written = body.Memories[0].Content
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"written": 1})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		}
	})
	s := reviewStore(t)
	if err := s.CreateReview(store.StoredReview{ID: "rv-3", Namespace: "ns", TargetKind: "memory",
		TargetID: "facts/operator/basil.md", DedupKey: "k3", Question: "Is basil still watered on Sundays?"}); err != nil {
		t.Fatal(err)
	}
	tool := &ReviewResolveTool{Store: s, MemoryMgr: m, AgentID: "nexus", Namespace: "ns"}
	res, err := tool.Execute(context.Background(), map[string]any{"id": "rv-3", "answer": "Wednesdays now",
		"resolution": "updated", "new_content": "Basil is watered on Wednesdays."})
	if err != nil || res.IsError {
		t.Fatalf("Execute = %v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(written, "Wednesdays") {
		t.Fatalf("the correction was not written:\n%s", written)
	}
	if !strings.Contains(written, "Basil gets fed monthly.") {
		t.Errorf("correcting one fact erased another:\n%s", written)
	}
}
