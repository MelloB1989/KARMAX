package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/dashboards"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// newDashboardsTestServer isolates dashboards.Root() (and the recipes
// directory a refresh would write into) under a fresh temp dir, so these
// tests never touch the operator's real ~/.karmax — the same isolation
// internal/dashboards' own tests use, applied here because the handlers call
// straight through to that package rather than holding any state of their
// own.
func newDashboardsTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("KARMAX_DATA_DIR", t.TempDir())
	t.Setenv("KARMAX_RECIPES_DIR", t.TempDir())

	db, err := store.New(filepath.Join(t.TempDir(), "dash.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New("127.0.0.1:0", 0, "", "", nil, db, nil, nil, &config.KarmaxConfig{}, zap.NewNop())
}

func TestHandleListDashboardsEmpty(t *testing.T) {
	srv := newDashboardsTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/api/dashboards", nil)
	w := httptest.NewRecorder()
	srv.handleListDashboards(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body struct {
		Dashboards []dashboards.Meta `json:"dashboards"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Dashboards == nil || len(body.Dashboards) != 0 {
		t.Fatalf("dashboards = %+v, want an empty (not null) list", body.Dashboards)
	}
}

func TestHandleGetDashboardUnknownIs404(t *testing.T) {
	srv := newDashboardsTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/api/dashboards/nope", nil)
	r.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	srv.handleGetDashboard(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestHandleGetDashboardInvalidIDIs400(t *testing.T) {
	srv := newDashboardsTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/api/dashboards/_kit", nil)
	r.SetPathValue("id", "_kit")
	w := httptest.NewRecorder()
	srv.handleGetDashboard(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestHandleGetDashboardReturnsMetaAndHTML(t *testing.T) {
	srv := newDashboardsTestServer(t)
	if _, err := dashboards.Save(dashboards.SaveInput{
		ID: "sales", Title: "Sales", HTMLSet: true, HTML: "<p>hi</p>",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/dashboards/sales", nil)
	r.SetPathValue("id", "sales")
	w := httptest.NewRecorder()
	srv.handleGetDashboard(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body struct {
		Dashboard dashboards.Meta `json:"dashboard"`
		HTML      string          `json:"html"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.HTML != "<p>hi</p>" || body.Dashboard.ID != "sales" {
		t.Fatalf("body = %+v", body)
	}
}

func TestHandleGetDashboardDataRoundTripAnd404s(t *testing.T) {
	srv := newDashboardsTestServer(t)
	if _, err := dashboards.Save(dashboards.SaveInput{
		ID: "d1", Title: "D1", HTMLSet: true, HTML: "<p></p>",
		Data: map[string]any{"revenue": map[string]any{"total": 42}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/dashboards/d1/data/revenue", nil)
	r.SetPathValue("id", "d1")
	r.SetPathValue("name", "revenue")
	w := httptest.NewRecorder()
	srv.handleGetDashboardData(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if strings.TrimSpace(w.Body.String()) != `{"total":42}` {
		t.Fatalf("body = %q", w.Body.String())
	}

	// Unknown data file on a real dashboard.
	r = httptest.NewRequest(http.MethodGet, "/api/dashboards/d1/data/nope", nil)
	r.SetPathValue("id", "d1")
	r.SetPathValue("name", "nope")
	w = httptest.NewRecorder()
	srv.handleGetDashboardData(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown data file: status = %d, want 404", w.Code)
	}

	// Unknown dashboard entirely.
	r = httptest.NewRequest(http.MethodGet, "/api/dashboards/nope/data/revenue", nil)
	r.SetPathValue("id", "nope")
	r.SetPathValue("name", "revenue")
	w = httptest.NewRecorder()
	srv.handleGetDashboardData(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown dashboard: status = %d, want 404", w.Code)
	}

	// Malformed name.
	r = httptest.NewRequest(http.MethodGet, "/api/dashboards/d1/data/Bad%20Name", nil)
	r.SetPathValue("id", "d1")
	r.SetPathValue("name", "Bad Name")
	w = httptest.NewRecorder()
	srv.handleGetDashboardData(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed name: status = %d, want 400", w.Code)
	}
}

func TestHandlePatchDashboardHappyPath(t *testing.T) {
	srv := newDashboardsTestServer(t)
	if _, err := dashboards.Save(dashboards.SaveInput{
		ID: "sales", Title: "Sales", HTMLSet: true, HTML: "<p></p>",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r := httptest.NewRequest(http.MethodPatch, "/api/dashboards/sales", strings.NewReader(`{"pinned":true,"archived":true}`))
	r.SetPathValue("id", "sales")
	w := httptest.NewRecorder()
	srv.auth(srv.handlePatchDashboard)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body struct {
		Dashboard dashboards.Meta `json:"dashboard"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Dashboard.Pinned || !body.Dashboard.Archived || body.Dashboard.ArchivedAt == "" {
		t.Fatalf("dashboard = %+v", body.Dashboard)
	}
}

func TestHandlePatchDashboardBadRequests(t *testing.T) {
	srv := newDashboardsTestServer(t)
	if _, err := dashboards.Save(dashboards.SaveInput{
		ID: "sales", Title: "Sales", HTMLSet: true, HTML: "<p></p>",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	patch := func(id, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPatch, "/api/dashboards/"+id, strings.NewReader(body))
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		srv.auth(srv.handlePatchDashboard)(w, r)
		return w
	}

	if w := patch("sales", `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty body: status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if w := patch("sales", `{"color":"red"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if w := patch("sales", `{"pinned":"yes"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong type: status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if w := patch("_kit", `{"pinned":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id: status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestHandlePatchDashboardUnknownIs404(t *testing.T) {
	srv := newDashboardsTestServer(t)
	r := httptest.NewRequest(http.MethodPatch, "/api/dashboards/nope", strings.NewReader(`{"pinned":true}`))
	r.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	srv.auth(srv.handlePatchDashboard)(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

// The harness's scoped token builds dashboards through the tool; pinning and
// archiving are the operator's own call and must stay out of its reach, the
// same as delete and the reference write.
func TestScopedTokenCannotPatchDashboards(t *testing.T) {
	t.Setenv("KARMAX_DATA_DIR", t.TempDir())
	t.Setenv("KARMAX_RECIPES_DIR", t.TempDir())
	srv := newScopeTestServer(t, "full-tok", "browser-tok")
	if _, err := dashboards.Save(dashboards.SaveInput{ID: "sales", Title: "Sales", HTML: "<p>x</p>", HTMLSet: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	patch := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPatch, "/api/dashboards/sales", strings.NewReader(`{"pinned":true}`))
		r.SetPathValue("id", "sales")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		srv.auth(srv.handlePatchDashboard)(w, r)
		return w
	}

	if w := patch("browser-tok"); w.Code != http.StatusForbidden {
		t.Fatalf("scoped patch: status = %d, want 403: %s", w.Code, w.Body.String())
	}
	meta, _, err := dashboards.Get("sales")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if meta.Pinned {
		t.Fatalf("a refused patch still pinned the dashboard: %+v", meta)
	}

	if w := patch("full-tok"); w.Code != http.StatusOK {
		t.Fatalf("full-token patch: status = %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleDeleteDashboard(t *testing.T) {
	srv := newDashboardsTestServer(t)
	if _, err := dashboards.Save(dashboards.SaveInput{
		ID: "gone-soon", Title: "T", HTMLSet: true, HTML: "<p></p>",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r := httptest.NewRequest(http.MethodDelete, "/api/dashboards/gone-soon", nil)
	r.SetPathValue("id", "gone-soon")
	w := httptest.NewRecorder()
	srv.auth(srv.handleDeleteDashboard)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	// Deleting again: the id parses fine but now names nothing.
	w = httptest.NewRecorder()
	srv.auth(srv.handleDeleteDashboard)(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("second delete: status = %d, want 404", w.Code)
	}
}

func TestHandlePutDashboardReference(t *testing.T) {
	srv := newDashboardsTestServer(t)

	r := httptest.NewRequest(http.MethodPut, "/api/dashboards/_kit/reference", strings.NewReader("# Kit\n\nUse <kit-card>."))
	w := httptest.NewRecorder()
	srv.auth(srv.handlePutDashboardReference)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := dashboards.ComponentReference(); got != "# Kit\n\nUse <kit-card>." {
		t.Fatalf("ComponentReference() = %q, want the text just installed", got)
	}

	// Over the limit is rejected, and must not have overwritten what was
	// just installed above.
	r = httptest.NewRequest(http.MethodPut, "/api/dashboards/_kit/reference", strings.NewReader(strings.Repeat("x", dashboardReferenceLimit+1)))
	w = httptest.NewRecorder()
	srv.auth(srv.handlePutDashboardReference)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized reference: status = %d, want 400", w.Code)
	}
	if got := dashboards.ComponentReference(); got != "# Kit\n\nUse <kit-card>." {
		t.Fatalf("a rejected write changed the installed reference: %q", got)
	}
}

// The routes themselves require the operator's full token like the rest of
// the API — a browser-scoped caller reaches dashboards only through the
// "dashboard" tool, never these routes directly.
func TestDashboardRoutesAreRegisteredUnderAuth(t *testing.T) {
	t.Setenv("KARMAX_DATA_DIR", t.TempDir())
	t.Setenv("KARMAX_RECIPES_DIR", t.TempDir())
	db, err := store.New(filepath.Join(t.TempDir(), "dash2.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := New("127.0.0.1:0", 0, "full-tok", "", nil, db, nil, nil, &config.KarmaxConfig{}, zap.NewNop())

	r := httptest.NewRequest(http.MethodGet, "/api/dashboards", nil)
	w := httptest.NewRecorder()
	srv.auth(srv.handleListDashboards)(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", w.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "/api/dashboards", nil)
	r.Header.Set("Authorization", "Bearer full-tok")
	w = httptest.NewRecorder()
	srv.auth(srv.handleListDashboards)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("full token: status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// The harness's scoped token builds dashboards through the tool; it must not
// delete one directly, or rewrite the reference every other agent reads.
func TestScopedTokenCannotDeleteDashboardsOrRewriteTheReference(t *testing.T) {
	t.Setenv("KARMAX_DATA_DIR", t.TempDir())
	t.Setenv("KARMAX_RECIPES_DIR", t.TempDir())
	srv := newScopeTestServer(t, "full-tok", "browser-tok")
	if _, err := dashboards.Save(dashboards.SaveInput{ID: "sales", Title: "Sales", HTML: "<p>x</p>", HTMLSet: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	del := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodDelete, "/api/dashboards/sales", nil)
		r.SetPathValue("id", "sales")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		srv.auth(srv.handleDeleteDashboard)(w, r)
		return w
	}
	put := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/api/dashboards/_kit/reference", strings.NewReader("# planted"))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		srv.auth(srv.handlePutDashboardReference)(w, r)
		return w
	}

	if w := del("browser-tok"); w.Code != http.StatusForbidden {
		t.Fatalf("scoped delete: status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if _, _, err := dashboards.Get("sales"); err != nil {
		t.Fatalf("a refused delete still removed the dashboard: %v", err)
	}
	if w := put("browser-tok"); w.Code != http.StatusForbidden {
		t.Fatalf("scoped reference write: status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if ref := dashboards.ComponentReference(); strings.Contains(ref, "planted") {
		t.Fatalf("a refused write still replaced the reference: %q", ref)
	}

	if w := put("full-tok"); w.Code != http.StatusOK {
		t.Fatalf("full-token reference write: status = %d: %s", w.Code, w.Body.String())
	}
	if w := del("full-tok"); w.Code != http.StatusOK {
		t.Fatalf("full-token delete: status = %d: %s", w.Code, w.Body.String())
	}
}
