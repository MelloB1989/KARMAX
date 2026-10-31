package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
)

// An answer from the app to a question that expired before the operator got
// to it is kept, not dropped as "already resolved".
func TestTheAppTakesALateAnswerToAnExpiredQuestion(t *testing.T) {
	srv := newTaskTestServer(t)
	if err := srv.store.CreateReview(store.StoredReview{ID: "rv-1", Namespace: "nexus", TargetKind: "reminder",
		DedupKey: "k1", Question: "Is the parcel still on its way?", CreatedAt: time.Now().Add(-10 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.ExpireReviews("nexus", time.Now().Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/reviews/rv-1/answer",
		strings.NewReader(`{"answer":"it came yesterday","resolution":"done"}`))
	r.SetPathValue("id", "rv-1")
	w := httptest.NewRecorder()
	srv.handleAnswerReview(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	rev, _ := srv.store.GetReview("rv-1")
	if rev.Answer != "it came yesterday" || rev.Resolution != "done" {
		t.Errorf("the late answer was dropped: status %q, resolution %q, answer %q", rev.Status, rev.Resolution, rev.Answer)
	}
}
