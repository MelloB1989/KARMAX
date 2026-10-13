package fakegithub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBranchProtectionReadsBackWhatWasSet(t *testing.T) {
	s := New("http://fake")
	s.AddRepo("acme/svc", "d", nil)
	url := "/repos/acme/svc/branches/main/protection"
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		return w
	}
	if w := get(); w.Code != http.StatusNotFound {
		t.Fatalf("unprotected branch: %d, want 404", w.Code)
	}
	body := `{"required_status_checks":{"strict":true,"contexts":["ci"]},"enforce_admins":false,
		"required_pull_request_reviews":{"required_approving_review_count":1},"restrictions":null}`
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("PUT", url, strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("PUT: %d", w.Code)
	}
	w = get()
	if w.Code != 200 {
		t.Fatalf("GET: %d %s", w.Code, w.Body)
	}
	var got struct {
		RSC struct {
			Strict   bool     `json:"strict"`
			Contexts []string `json:"contexts"`
		} `json:"required_status_checks"`
		PR struct {
			N int `json:"required_approving_review_count"`
		} `json:"required_pull_request_reviews"`
		Admins struct {
			Enabled bool `json:"enabled"`
		} `json:"enforce_admins"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.RSC.Strict || len(got.RSC.Contexts) != 1 || got.RSC.Contexts[0] != "ci" || got.PR.N != 1 || got.Admins.Enabled {
		t.Fatalf("read back %s", w.Body)
	}
}
