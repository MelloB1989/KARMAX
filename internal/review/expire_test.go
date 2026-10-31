package review

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
)

// Eight questions nobody answered held every slot, and the reviewer asked
// nothing new for seven weeks. A question unanswered for long enough expires,
// and the loop goes on asking.
func TestUnansweredQuestionsExpireAndTheLoopAsksAgain(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/retrieve") {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"memories": []any{
			map[string]any{"path": "facts/a.md", "tier": "facts", "updated_at": old,
				"content": "Omer will get back by Friday about the deadline"},
		}})
	}))
	defer srv.Close()

	r := reviewerWithGitLoom(t, srv.URL)
	r.cfg.ExpireAfter = 7 * 24 * time.Hour
	asked := 0
	r.cfg.Ask = func(ctx context.Context, key, prompt string) (string, bool) {
		asked++
		return `{"idx":0,"question":"Did Omer get back to you about the Friday deadline?","options":["Done","Still open"]}`, true
	}

	var stale []string
	for i := 0; i < maxOpenReviews; i++ {
		id := fmt.Sprintf("stale-%d", i)
		stale = append(stale, id)
		if err := r.store.CreateReview(store.StoredReview{ID: id, Namespace: "test-ns", TargetKind: "reminder",
			DedupKey: id, Question: fmt.Sprintf("Is parcel number %d still on its way?", i),
			CreatedAt: time.Now().Add(-10 * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.store.CreateReview(store.StoredReview{ID: "recent", Namespace: "test-ns", TargetKind: "reminder",
		DedupKey: "recent", Question: "Is the plumber still coming on Monday?",
		CreatedAt: time.Now().Add(-2 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, id := range stale {
		rev, err := r.store.GetReview(id)
		if err != nil || rev == nil {
			t.Fatalf("GetReview(%s) = %v, %v", id, rev, err)
		}
		if rev.Status != "dismissed" || rev.Resolution != store.ResolutionExpired || rev.Answer != "" {
			t.Errorf("%s: status %q, resolution %q, answer %q; want dismissed, expired, no answer",
				id, rev.Status, rev.Resolution, rev.Answer)
		}
	}
	if rev, _ := r.store.GetReview("recent"); rev == nil || rev.Status != "open" {
		t.Errorf("a two-day-old question expired: %+v", rev)
	}
	if asked != 1 {
		t.Errorf("the judge was asked %d times, want 1: the loop must go on asking", asked)
	}
	if n, _ := r.store.CountOpenReviews("test-ns"); n != 2 {
		t.Errorf("%d questions open, want the recent one and the new one", n)
	}
}

// A loop's log stops changing when its month ends. That is not a fact gone
// stale, and the operator is not asked whether last month's log still holds.
func TestALoopsLogIsNotAStalenessCandidate(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/retrieve") {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"memories": []any{
			map[string]any{"path": "facts/context/night-watch/2026-09.md", "tier": "facts", "updated_at": old,
				"content": "(night-watch, 29 Sep) group-a [reminded]: still waiting on Omer for the Friday call"},
			map[string]any{"path": "facts/a.md", "tier": "facts", "updated_at": old,
				"content": "Omer will get back by Friday about the deadline"},
		}})
	}))
	defer srv.Close()

	r := reviewerWithGitLoom(t, srv.URL)
	got := r.staleMemories(context.Background(), time.Now().Add(-4*24*time.Hour))
	if len(got) != 1 || got[0].id != "facts/a.md" {
		t.Errorf("candidates = %+v, want only the fact", got)
	}
}

func TestExpiryDefaultsToAWeekAndCanBeTurnedOff(t *testing.T) {
	if got := (&Reviewer{}).expireAfter(); got != 7*24*time.Hour {
		t.Errorf("default expiry = %v, want a week", got)
	}
	r := reviewerWithGitLoom(t, "http://127.0.0.1:1")
	r.cfg.ExpireAfter = -1
	if err := r.store.CreateReview(store.StoredReview{ID: "ancient", Namespace: "test-ns", TargetKind: "reminder",
		DedupKey: "ancient", Question: "Is the parcel still on its way?",
		CreatedAt: time.Now().Add(-90 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r.expire()
	if rev, _ := r.store.GetReview("ancient"); rev == nil || rev.Status != "open" {
		t.Errorf("a question expired with expiry turned off: %+v", rev)
	}
}
