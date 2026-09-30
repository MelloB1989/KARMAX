package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func backendAt(url string) *gitloomBackend {
	return &gitloomBackend{
		cfg: GitLoomConfig{BaseURL: url, APIKey: "k", Namespace: "karmax", Timeout: 5 * time.Second},
		log: zap.NewNop(),
	}
}

// The live response shape since 2026-09-15, trimmed from a real /v1/retrieve.
const liveShape = `{
  "namespace": "karmax", "query": "CampX", "mode": "raw", "candidates": 12, "filtered_out": 9, "millis": 870,
  "memories": [
    {"path": "facts/campx-cloud-infrastructure/campx-azure.md", "tier": "facts",
     "title": "CampX cloud is hosted on Azure", "content": "[2026-08-24] CampX cloud is hosted on Azure (Shiva's setup), not AWS.",
     "snippet": "", "score": 1, "matched": ["lexical"], "created": "2026-08-24T10:00:00Z",
     "related": [{"label": "owner", "path": "facts/people/shiva.md", "snippet": "Shiva runs CampX infra"}]}
  ],
  "defined": [{"path": "vocab/campx.md", "term": "CampX", "definition": "A client; TrustStrike runs its VAPT."}]
}`

func TestRecallReadsTheLiveShape(t *testing.T) {
	var gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		if r.URL.Path != "/v1/retrieve" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(liveShape))
	}))
	defer srv.Close()

	res, err := backendAt(srv.URL).search(context.Background(), "CampX", 5)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth = %q", gotAuth)
	}
	for _, want := range []string{"q=CampX", "namespace=karmax", "limit=5", "no_provenance=1"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q is missing %s", gotQuery, want)
		}
	}
	// The whole point: the memory the server returned is the memory KARMAX sees.
	if len(res) != 2 {
		t.Fatalf("got %d results, want the defined term and the memory", len(res))
	}
	if !strings.HasPrefix(res[0].Entry.Content, "CampX: A client") {
		t.Errorf("defined vocabulary should lead, got %q", res[0].Entry.Content)
	}
	m := res[1]
	if !strings.Contains(m.Entry.Content, "hosted on Azure") {
		t.Errorf("content = %q", m.Entry.Content)
	}
	if !strings.Contains(m.Entry.Content, "↳ owner: Shiva runs CampX infra") {
		t.Errorf("relations were not carried: %q", m.Entry.Content)
	}
	if m.Excerpt == "" {
		t.Error("an empty snippet left an empty excerpt — the exact failure that emptied every call-time lookup")
	}
	if m.Entry.CreatedAt.IsZero() {
		t.Error("created date was lost")
	}
	if m.Entry.ID != "facts/campx-cloud-infrastructure/campx-azure.md" {
		t.Errorf("id = %q, want the path (it is the Forget handle)", m.Entry.ID)
	}
}

// Silence is what hid the last shape change for two weeks. A response in the
// old shape must fail, not decode as an empty memory.
func TestTheOldShapeFailsLoudly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"namespace":"karmax","hits":[{"path":"a.md","score":0.9,"snippet":"x"}]}`))
	}))
	defer srv.Close()
	if _, err := backendAt(srv.URL).search(context.Background(), "x", 5); err == nil {
		t.Fatal("a response in the old shape decoded silently")
	}
}

func TestNoMatchesIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"namespace":"karmax","memories":[],"candidates":0}`))
	}))
	defer srv.Close()
	res, err := backendAt(srv.URL).search(context.Background(), "nothing", 5)
	if err != nil || len(res) != 0 {
		t.Fatalf("res=%v err=%v, want an empty result and no error", res, err)
	}
}

func TestServerErrorsAreReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"quota_exceeded"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := backendAt(srv.URL).search(context.Background(), "x", 5)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want the status reported", err)
	}
}
