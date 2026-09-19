package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/loopinstall"
	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/wasmloop"
	"go.uber.org/zap"
)

// Handler tests for the registry API: a fake registry (httptest) stands in
// for github.com/MelloB1989/karmax-loops, and every KARMAX-side directory
// (recipes, signed loops, the disabled-loops list) is pointed at a fresh
// t.TempDir() per test via env vars, the same knobs the CLI honors.

const goodRecipeYAML = "name: good-recipe\non:\n  schedule: \"0 9 * * *\"\nsteps:\n  - log: \"hi\"\n"

// newRegistryTestServer builds a Server backed by a real (temp-file) store,
// with auth disabled — these tests are about the registry logic, not the
// bearer gate every other handler already covers — and every on-disk
// location the registry package touches redirected under t.TempDir().
func newRegistryTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "registry.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	t.Setenv("KARMAX_RECIPES_DIR", filepath.Join(t.TempDir(), "recipes"))
	t.Setenv("KARMAX_LOOPS_DIR", filepath.Join(t.TempDir(), "loops"))
	t.Setenv("KARMAX_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	// The fake registry runs on 127.0.0.1, which safety.CheckURL refuses by
	// default (loopback is where KARMAX's own APIs live) — the same guard a
	// loop's own egress passes. A registry fetch in production always hits a
	// real host, so this is a test-only relaxation, not a change in what a
	// loop may reach.
	t.Setenv("KARMAX_ALLOW_PRIVATE_HTTP", "true")

	return New("127.0.0.1:0", 0, "", "", nil, db, nil, nil, &config.KarmaxConfig{}, zap.NewNop())
}

// newTestRegistry serves index.json plus whatever files are given, and points
// KARMAX_REGISTRY at it for the rest of the test.
func newTestRegistry(t *testing.T, entries []wasmloop.RegistryEntry, files map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wasmloop.Index{Version: 1, Entries: entries})
	})
	for path, body := range files {
		body := body
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) })
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KARMAX_REGISTRY", srv.URL)
	return srv
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// defaultFixture is a registry with one installable recipe, one whose index
// entry lies about its digest, and one workflow signed by nobody — enough to
// cover every status this file tests.
func defaultFixture(t *testing.T) (goodRecipe, badRecipe, workflow wasmloop.RegistryEntry, workflowData []byte) {
	t.Helper()
	goodBody := []byte(goodRecipeYAML)

	m := wasmloop.Manifest{Name: "untrusted-flow", Version: "1.0.0", Description: "an unsigned test workflow", Schedule: "0 */6 * * *"}
	data, err := wasmloop.Pack(m, []byte("\x00asm pretend module"), nil)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	goodRecipe = wasmloop.RegistryEntry{
		Name: "good-recipe", Kind: wasmloop.KindRecipe, Version: "1.0.0",
		Description: "a test recipe", Author: "tester",
		Source: "recipes/good-recipe", Artifact: "/good-recipe.yaml",
		SHA256: sha256Hex(goodBody), Requires: []string{"whatsapp"},
	}
	badRecipe = wasmloop.RegistryEntry{
		Name: "bad-recipe", Kind: wasmloop.KindRecipe, Version: "1.0.0",
		Description: "index lies about the digest", Author: "tester",
		Artifact: "/good-recipe.yaml", SHA256: strings.Repeat("0", 64),
	}
	workflow = wasmloop.RegistryEntry{
		Name: "untrusted-flow", Kind: wasmloop.KindWorkflow, Version: "1.0.0",
		Description: "an unsigned test workflow", Author: "tester",
		Source: "workflows/untrusted-flow", Artifact: "/untrusted-flow.kloop",
		SHA256: sha256Hex(data),
	}
	return goodRecipe, badRecipe, workflow, data
}

func startFixtureRegistry(t *testing.T) (goodRecipe, badRecipe, workflow wasmloop.RegistryEntry) {
	t.Helper()
	goodRecipe, badRecipe, workflow, workflowData := defaultFixture(t)
	newTestRegistry(t, []wasmloop.RegistryEntry{goodRecipe, badRecipe, workflow}, map[string][]byte{
		"/good-recipe.yaml":                   []byte(goodRecipeYAML),
		"/untrusted-flow.kloop":               workflowData,
		"/workflows/untrusted-flow/loop.yaml": []byte("name: untrusted-flow\nversion: 1.0.0\nschedule: \"0 */6 * * *\"\ntools:\n  - example.tool\nhost:\n  - log\n"),
	})
	return goodRecipe, badRecipe, workflow
}

func doJSON(t *testing.T, h http.HandlerFunc, method, path, name string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if name != "" {
		r.SetPathValue("name", name)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, w.Body.String())
	}
	return out
}

func TestRegistryList_InstalledAndActiveFlags(t *testing.T) {
	srv := newRegistryTestServer(t)
	good, _, _ := startFixtureRegistry(t)

	// Not installed, not active yet.
	w := doJSON(t, srv.handleLoopsRegistryList, http.MethodGet, "/api/loops/registry", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	entries, _ := body["entries"].([]any)
	view := findEntry(t, entries, good.Name)
	if view["installed"] != false || view["active"] != false {
		t.Errorf("fresh entry = %+v, want installed:false active:false", view)
	}
	if _, ok := body["fetchedAt"].(string); !ok {
		t.Errorf("fetchedAt missing: %+v", body)
	}
	if src, _ := body["source"].(string); !strings.HasSuffix(src, "/index.json") {
		t.Errorf("source = %v, want it to end in /index.json", body["source"])
	}

	// Install it, and wire it into the "live" loop list — both flags flip.
	installW := doJSON(t, srv.handleLoopsRegistryInstall, http.MethodPost, "/api/loops/registry/"+good.Name+"/install", good.Name, nil)
	if installW.Code != http.StatusOK {
		t.Fatalf("install status = %d: %s", installW.Code, installW.Body.String())
	}
	srv.SetListLoops(func() []LoopInfo { return []LoopInfo{{Name: good.Name, Kind: "recipe", Enabled: true}} })

	w2 := doJSON(t, srv.handleLoopsRegistryList, http.MethodGet, "/api/loops/registry", "", nil)
	body2 := decodeBody(t, w2)
	entries2, _ := body2["entries"].([]any)
	view2 := findEntry(t, entries2, good.Name)
	if view2["installed"] != true || view2["active"] != true {
		t.Errorf("installed+active entry = %+v, want both true", view2)
	}
	reqs, _ := view2["requires"].([]any)
	if len(reqs) != 1 || reqs[0] != "whatsapp" {
		t.Errorf("requires = %v, want [whatsapp]", view2["requires"])
	}
}

func findEntry(t *testing.T, entries []any, name string) map[string]any {
	t.Helper()
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if ok && m["name"] == name {
			return m
		}
	}
	t.Fatalf("no entry named %q in %+v", name, entries)
	return nil
}

func TestRegistryDetail_DigestMismatchIs502AndNeverServed(t *testing.T) {
	srv := newRegistryTestServer(t)
	_, bad, _ := startFixtureRegistry(t)

	w := doJSON(t, srv.handleLoopsRegistryDetail, http.MethodGet, "/api/loops/registry/"+bad.Name, bad.Name, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "on: schedule") {
		t.Errorf("a digest-mismatched artifact's content leaked into the response: %s", w.Body.String())
	}
}

func TestRegistryDetail_RecipeReturnsDefinitionTriggerAndTools(t *testing.T) {
	srv := newRegistryTestServer(t)
	good, _, _ := startFixtureRegistry(t)

	w := doJSON(t, srv.handleLoopsRegistryDetail, http.MethodGet, "/api/loops/registry/"+good.Name, good.Name, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["definition"] != goodRecipeYAML {
		t.Errorf("definition = %q", body["definition"])
	}
	// recipes.Parse normalises a 5-field cron to robfig's 6-field
	// (seconds-first) form — the trigger reads back as the recipe now
	// states it, not as the registry's YAML happened to spell it.
	if body["trigger"] != "0 0 9 * * *" {
		t.Errorf("trigger = %q, want the normalised cron string", body["trigger"])
	}
	if _, ok := body["tools"].([]any); !ok {
		t.Errorf("tools should be an array (possibly empty), got %T", body["tools"])
	}
	if host, ok := body["host"].([]any); !ok || len(host) != 0 {
		t.Errorf("host = %v, want an empty array for a recipe", body["host"])
	}
}

// A workflow's detail view reads its companion loop.yaml, not the .kloop
// artifact — this is the only test that exercises that fetch.
func TestRegistryDetail_WorkflowReadsCompanionManifest(t *testing.T) {
	srv := newRegistryTestServer(t)
	_, _, workflow := startFixtureRegistry(t)

	w := doJSON(t, srv.handleLoopsRegistryDetail, http.MethodGet, "/api/loops/registry/"+workflow.Name, workflow.Name, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if !strings.Contains(body["definition"].(string), "untrusted-flow") {
		t.Errorf("definition = %q, want the fetched loop.yaml text", body["definition"])
	}
	if body["trigger"] != "0 */6 * * *" {
		t.Errorf("trigger = %q, want the manifest's own schedule string", body["trigger"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0] != "example.tool" {
		t.Errorf("tools = %v, want [example.tool]", body["tools"])
	}
	host, _ := body["host"].([]any)
	if len(host) != 1 || host[0] != "log" {
		t.Errorf("host = %v, want [log]", body["host"])
	}
}

func TestRegistryDetail_UnknownNameIs404(t *testing.T) {
	srv := newRegistryTestServer(t)
	startFixtureRegistry(t)

	w := doJSON(t, srv.handleLoopsRegistryDetail, http.MethodGet, "/api/loops/registry/nope", "nope", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestRegistryInstallRecipe_WritesAndIsIdempotent(t *testing.T) {
	srv := newRegistryTestServer(t)
	good, _, _ := startFixtureRegistry(t)

	for i := 0; i < 2; i++ {
		w := doJSON(t, srv.handleLoopsRegistryInstall, http.MethodPost, "/api/loops/registry/"+good.Name+"/install", good.Name, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("install #%d status = %d: %s", i+1, w.Code, w.Body.String())
		}
		body := decodeBody(t, w)
		if body["installed"] != true {
			t.Errorf("install #%d: installed = %v", i+1, body["installed"])
		}
		if body["restartRequired"] != false {
			t.Errorf("install #%d: a recipe should never require a restart, got %v", i+1, body["restartRequired"])
		}
	}

	data, err := os.ReadFile(filepath.Join(recipes.Dir(), good.Name+".yaml"))
	if err != nil {
		t.Fatalf("recipe was not written: %v", err)
	}
	if string(data) != goodRecipeYAML {
		t.Errorf("written recipe = %q", data)
	}
}

func TestRegistryUninstallRecipe_RemovesTheFile(t *testing.T) {
	srv := newRegistryTestServer(t)
	good, _, _ := startFixtureRegistry(t)

	if w := doJSON(t, srv.handleLoopsRegistryInstall, http.MethodPost, "/api/loops/registry/"+good.Name+"/install", good.Name, nil); w.Code != http.StatusOK {
		t.Fatalf("install: status = %d: %s", w.Code, w.Body.String())
	}

	w := doJSON(t, srv.handleLoopsRegistryUninstall, http.MethodDelete, "/api/loops/registry/"+good.Name, good.Name, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("uninstall status = %d: %s", w.Code, w.Body.String())
	}
	if decodeBody(t, w)["removed"] != true {
		t.Errorf("removed = %v", decodeBody(t, w)["removed"])
	}
	if _, err := os.Stat(filepath.Join(recipes.Dir(), good.Name+".yaml")); !os.IsNotExist(err) {
		t.Errorf("recipe file still exists after uninstall: err=%v", err)
	}

	// Uninstalling again: it is no longer here.
	w2 := doJSON(t, srv.handleLoopsRegistryUninstall, http.MethodDelete, "/api/loops/registry/"+good.Name, good.Name, nil)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("second uninstall status = %d, want 404: %s", w2.Code, w2.Body.String())
	}
}

// A recipe KARMAX ships embedded (see recipes.BuiltinNames) must refuse
// removal through the registry API even though, mechanically, it is just
// another file in the same directory a registry install would write to.
func TestRegistryUninstall_ShippedBuiltinIsConflict(t *testing.T) {
	srv := newRegistryTestServer(t)
	startFixtureRegistry(t)

	builtins := recipes.BuiltinNames()
	if len(builtins) == 0 {
		t.Skip("no embedded builtin recipes to test against")
	}
	name := builtins[0]
	// InstallBuiltins is what normally puts these on disk; reproduce that
	// narrowly rather than pulling in the whole embed for one test.
	if err := os.MkdirAll(recipes.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recipes.Dir(), name+".yaml"), []byte("name: "+name+"\non:\n  manual: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := doJSON(t, srv.handleLoopsRegistryUninstall, http.MethodDelete, "/api/loops/registry/"+name, name, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(recipes.Dir(), name+".yaml")); err != nil {
		t.Errorf("a refused uninstall must not remove the file: %v", err)
	}
}

func TestRegistryInvalidNameIs400(t *testing.T) {
	srv := newRegistryTestServer(t)
	startFixtureRegistry(t)

	bad := "Not_A-Valid.Name!"
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		m    string
	}{
		{"detail", srv.handleLoopsRegistryDetail, http.MethodGet},
		{"install", srv.handleLoopsRegistryInstall, http.MethodPost},
		{"uninstall", srv.handleLoopsRegistryUninstall, http.MethodDelete},
		{"enable", srv.handleEnableLoop, http.MethodPost},
		{"disable", srv.handleDisableLoop, http.MethodPost},
	} {
		w := doJSON(t, tc.h, tc.m, "/api/loops/registry/"+bad, bad, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", tc.name, w.Code, w.Body.String())
		}
	}
}

func TestRegistryInstall_UnknownNameIs404(t *testing.T) {
	srv := newRegistryTestServer(t)
	startFixtureRegistry(t)

	w := doJSON(t, srv.handleLoopsRegistryInstall, http.MethodPost, "/api/loops/registry/nope/install", "nope", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

// A workflow nobody signed can only be installed with allowUntrusted — the
// API's one boolean standing in for the CLI's confirmUnreviewed prompt.
//
// wasmloop's own test helpers (newSigner, packed, manifestFor in
// internal/wasmloop/*_test.go) are unexported and live in that package's own
// test binary, so this package cannot import them; the artifact here is
// built directly from wasmloop's public Manifest/Pack instead — unsigned, on
// purpose, since that is the simplest way to land below TierRegistry.
func TestRegistryInstallWorkflow_UntrustedRequiresTheFlag(t *testing.T) {
	srv := newRegistryTestServer(t)
	_, _, workflow := startFixtureRegistry(t)

	w := doJSON(t, srv.handleLoopsRegistryInstall, http.MethodPost, "/api/loops/registry/"+workflow.Name+"/install", workflow.Name, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["untrusted"] != true {
		t.Errorf("untrusted flag missing from refusal: %+v", body)
	}

	w2 := doJSON(t, srv.handleLoopsRegistryInstall, http.MethodPost, "/api/loops/registry/"+workflow.Name+"/install", workflow.Name,
		map[string]any{"allowUntrusted": true})
	if w2.Code != http.StatusOK {
		t.Fatalf("status with allowUntrusted = %d, want 200: %s", w2.Code, w2.Body.String())
	}
	body2 := decodeBody(t, w2)
	if body2["installed"] != true || body2["restartRequired"] != true {
		t.Errorf("install = %+v, want installed:true restartRequired:true", body2)
	}

	lock, err := wasmloop.LoadLock(wasmloop.Dir())
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	if _, ok := lock.Get(workflow.Name); !ok {
		t.Errorf("workflow was not recorded in the lockfile")
	}
}

func TestEnableDisableLoop(t *testing.T) {
	srv := newRegistryTestServer(t)
	good, _, _ := startFixtureRegistry(t)
	if w := doJSON(t, srv.handleLoopsRegistryInstall, http.MethodPost, "/api/loops/registry/"+good.Name+"/install", good.Name, nil); w.Code != http.StatusOK {
		t.Fatalf("install: status = %d: %s", w.Code, w.Body.String())
	}

	reapplied := 0
	srv.SetLoopsChanged(func() { reapplied++ })

	w := doJSON(t, srv.handleDisableLoop, http.MethodPost, "/api/loops/"+good.Name+"/disable", good.Name, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("disable status = %d: %s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["enabled"] != false || body["name"] != good.Name {
		t.Errorf("disable response = %+v", body)
	}
	// A recipe takes the change live, so it needs no restart and is re-applied.
	if body["restartRequired"] != false || reapplied != 1 {
		t.Errorf("recipe disable: restartRequired = %v, reapplied = %d; want false, 1", body["restartRequired"], reapplied)
	}
	if !loopinstall.LoadDisabledLoops()[good.Name] {
		t.Errorf("loopinstall does not show %s as disabled", good.Name)
	}

	w2 := doJSON(t, srv.handleEnableLoop, http.MethodPost, "/api/loops/"+good.Name+"/enable", good.Name, nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("enable status = %d: %s", w2.Code, w2.Body.String())
	}
	if decodeBody(t, w2)["enabled"] != true {
		t.Errorf("enable response = %+v", decodeBody(t, w2))
	}
	if loopinstall.LoadDisabledLoops()[good.Name] {
		t.Errorf("loopinstall still shows %s as disabled after enabling", good.Name)
	}

	w3 := doJSON(t, srv.handleEnableLoop, http.MethodPost, "/api/loops/never-installed/enable", "never-installed", nil)
	if w3.Code != http.StatusNotFound {
		t.Fatalf("enabling an unknown loop: status = %d, want 404: %s", w3.Code, w3.Body.String())
	}

	// A workflow is only read at start: the response has to say so, and there
	// is nothing to re-apply live.
	srv.SetListLoops(func() []LoopInfo { return []LoopInfo{{Name: "signed-watch", Kind: "workflow", Enabled: true}} })
	before := reapplied
	w4 := doJSON(t, srv.handleDisableLoop, http.MethodPost, "/api/loops/signed-watch/disable", "signed-watch", nil)
	if w4.Code != http.StatusOK {
		t.Fatalf("disabling a workflow: status = %d: %s", w4.Code, w4.Body.String())
	}
	if b := decodeBody(t, w4); b["restartRequired"] != true || reapplied != before {
		t.Errorf("workflow disable: restartRequired = %v, reapplied %d more times; want true, 0", b["restartRequired"], reapplied-before)
	}
}
