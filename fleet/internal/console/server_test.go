package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

const testToken = "client-token-0123456789"

func testServer(t *testing.T, assets map[string]string, fleetd *FleetdClient) (*httptest.Server, *Hub) {
	t.Helper()
	var n atomic.Int64
	h := newHub(countingFill(&n), 10*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.Run(ctx)
	fsys := fstest.MapFS{}
	for k, v := range assets {
		fsys[k] = &fstest.MapFile{Data: []byte(v)}
	}
	srv := httptest.NewServer(newHandler(h, &Terms{}, testToken, fleetd, fsys))
	t.Cleanup(srv.Close)
	return srv, h
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestLoginSetsCookieAndRedirectsWithoutTheToken(t *testing.T) {
	srv, _ := testServer(t, nil, nil)
	resp, err := noRedirect().Get(srv.URL + "/login?k=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/" {
		t.Fatalf("%d %q: the redirect must not carry the token", resp.StatusCode, resp.Header.Get("Location"))
	}
	var c *http.Cookie
	for _, x := range resp.Cookies() {
		if x.Name == "fc" {
			c = x
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Value != testToken {
		t.Fatalf("cookie = %+v", c)
	}
	// the cookie opens the API
	req, _ := http.NewRequest("GET", srv.URL+"/v1/snapshot", nil)
	req.AddCookie(c)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 200 {
		t.Fatalf("snapshot with cookie = %d", r.StatusCode)
	}
	if r, _ := noRedirect().Get(srv.URL + "/login?k=wrong"); r.StatusCode != 401 || len(r.Cookies()) != 0 {
		t.Fatalf("bad token login = %d", r.StatusCode)
	}
	if r, _ := noRedirect().Get(srv.URL + "/login"); r.StatusCode != 401 {
		t.Fatalf("no token login = %d", r.StatusCode)
	}
}

func TestAPIRefusesWithoutToken(t *testing.T) {
	srv, _ := testServer(t, nil, nil)
	for _, p := range []string{"/v1/snapshot", "/v1/watch", "/v1/term/agent-01", "/v1/nothing"} {
		if r, _ := http.Get(srv.URL + p); r.StatusCode != 401 {
			t.Errorf("GET %s = %d, want 401", p, r.StatusCode)
		}
	}
	for _, p := range []string{"/v1/prompt", "/v1/tell", "/v1/rotate"} {
		if r, _ := http.Post(srv.URL+p, "application/json", strings.NewReader("{}")); r.StatusCode != 401 {
			t.Errorf("POST %s = %d, want 401", p, r.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/snapshot", nil)
	req.Header.Set("Authorization", "Bearer nope")
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 401 {
		t.Errorf("wrong bearer = %d", r.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	r, _ := http.DefaultClient.Do(req)
	var s Snapshot
	if r.StatusCode != 200 || json.NewDecoder(r.Body).Decode(&s) != nil || len(s.Agents) != 4 {
		t.Fatalf("snapshot with bearer = %d", r.StatusCode)
	}
}

func TestStaticNeedsNoTokenButAPIDoes(t *testing.T) {
	srv, _ := testServer(t, map[string]string{"index.html": "<p>panel</p>", "assets/app.js": "console.log(1)"}, nil)
	get := func(p string) (int, string) {
		r, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	if c, b := get("/"); c != 200 || !strings.Contains(b, "panel") {
		t.Fatalf("/ = %d %q", c, b)
	}
	if c, b := get("/assets/app.js"); c != 200 || b != "console.log(1)" {
		t.Fatalf("asset = %d %q", c, b)
	}
	if c, b := get("/office"); c != 200 || !strings.Contains(b, "panel") {
		t.Fatalf("SPA fallback = %d %q", c, b)
	}
	if c, _ := get("/v1/snapshot"); c != 401 {
		t.Fatalf("api = %d", c)
	}
	if c, _ := get("/healthz"); c != 200 {
		t.Fatalf("healthz = %d", c)
	}
}

func TestPanelNotBuiltSaysSo(t *testing.T) {
	srv, _ := testServer(t, nil, nil) // no index.html
	r, _ := http.Get(srv.URL + "/")
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(b), "panel not built") || !strings.Contains(string(b), "npm run build") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	// the real embed (nothing built in a checkout) says the same
	if _, err := os.Stat("dist/index.html"); err != nil {
		rec := httptest.NewRecorder()
		static(embeddedAssets()).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if !strings.Contains(rec.Body.String(), "panel not built") {
			t.Fatalf("embedded: %s", rec.Body)
		}
	}
}

func TestHealthzHasSourcesAndVersion(t *testing.T) {
	srv, _ := testServer(t, nil, nil)
	r, _ := http.Get(srv.URL + "/healthz")
	var m struct {
		Sources map[string]Source
		Version string
	}
	if json.NewDecoder(r.Body).Decode(&m) != nil || m.Version == "" || len(m.Sources) == 0 {
		t.Fatalf("healthz = %+v", m)
	}
}

func TestWatchOverWebSocket(t *testing.T) {
	srv, h := testServer(t, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/watch",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + testToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	read := func() Message {
		typ, b, err := c.Read(ctx)
		if err != nil || typ != websocket.MessageText {
			t.Fatalf("read: %v %v", typ, err)
		}
		var m Message
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	first := read()
	if first.Type != "snapshot" || first.Snapshot == nil {
		t.Fatalf("first = %+v", first)
	}
	h.Changed()
	if p := read(); p.Type != "patch" || p.Seq != first.Seq+1 || len(p.Ops) == 0 {
		t.Fatalf("patch = %+v", p)
	}
	// without the token, no upgrade
	if _, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/watch", nil); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("unauthenticated watch: %v", err)
	}
}

func TestPromptIsProxiedAsIs(t *testing.T) {
	var gotBody, gotAuth, gotPath string
	fleetd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotAuth, gotPath = string(b), r.Header.Get("Authorization"), r.URL.Path
		w.WriteHeader(409)
		w.Write([]byte(`{"error":"agent-02 is busy"}`))
	}))
	defer fleetd.Close()
	tok := filepath.Join(t.TempDir(), "t")
	os.WriteFile(tok, []byte("fleetd-secret\n"), 0o600)
	srv, _ := testServer(t, nil, NewFleetdClient(fleetd.URL, tok))

	req, _ := http.NewRequest("POST", srv.URL+"/v1/prompt", strings.NewReader(`{"agent":"agent-02","text":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 409 || string(b) != `{"error":"agent-02 is busy"}` {
		t.Fatalf("reply = %d %s: fleetd's status and body, verbatim", r.StatusCode, b)
	}
	if gotPath != "/v1/prompt" || gotBody != `{"agent":"agent-02","text":"hi"}` || gotAuth != "Bearer fleetd-secret" {
		t.Fatalf("fleetd saw %s %q auth=%q (the client's token must not be forwarded)", gotPath, gotBody, gotAuth)
	}
}

func TestRefusesAWorldReadableTokenFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "client.token")
	os.WriteFile(p, []byte("abc\n"), 0o644)
	if _, err := ReadTokenFile(p); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("err = %v", err)
	}
	os.Chmod(p, 0o600)
	if tok, err := ReadTokenFile(p); err != nil || tok != "abc" {
		t.Fatalf("%q %v", tok, err)
	}
	os.WriteFile(p, []byte("\n"), 0o600)
	if _, err := ReadTokenFile(p); err == nil {
		t.Fatal("an empty token must be refused")
	}
}

func TestUnknownAPIPathIs404NotThePanel(t *testing.T) {
	srv, _ := testServer(t, map[string]string{"index.html": "<p>panel</p>"}, nil)
	for _, p := range []string{"/v1/nothing", "/v1/term", "/healthz/x"} {
		req, _ := http.NewRequest("GET", srv.URL+p, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		want := 404
		if p == "/healthz/x" {
			want = 200 // not an API path: the panel's own routing
		}
		if r.StatusCode != want || (want == 404 && (strings.Contains(string(b), "panel") || !strings.Contains(r.Header.Get("Content-Type"), "json"))) {
			t.Errorf("%s = %d %q", p, r.StatusCode, b)
		}
	}
	if r, _ := http.Get(srv.URL + "/v1/term/agent-01"); r.StatusCode != 401 {
		t.Errorf("unauthenticated unknown term = %d", r.StatusCode)
	}
}

func TestTermRouteControlHolderAndBytes(t *testing.T) {
	cfg := &Config{Term: TermSize{Cols: 80, Rows: 24}, Agents: []AgentConf{{Name: "a1", Live: true}}}
	terms := NewTerms(cfg, func([]string) (*os.File, *exec.Cmd, error) {
		cmd := exec.Command("cat")
		f, err := pty.Start(cmd)
		return f, cmd, err
	})
	var n atomic.Int64
	h := newHub(countingFill(&n), 10*time.Millisecond, nil)
	srv := httptest.NewServer(newHandler(h, terms, testToken, nil, fstest.MapFS{}))
	defer srv.Close()
	hdr := http.Header{"Authorization": {"Bearer " + testToken}}
	ws := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, ws+"/v1/term/a1?control=1&who=alice", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	var hello bool
	for !strings.Contains(got.String(), "ping") {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (got %q)", err, got.String())
		}
		if typ == websocket.MessageText {
			var f TermFrame
			json.Unmarshal(b, &f)
			hello = hello || f.Type == "hello" && f.Mode == "control" && f.Cols == 80
		} else {
			got.Write(b)
		}
	}
	if !hello {
		t.Fatal("no hello frame")
	}

	req, _ := http.NewRequest("GET", srv.URL+"/v1/term/a1?control=1&who=bob", nil)
	req.Header = hdr
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Holder string }
	json.NewDecoder(r.Body).Decode(&body)
	if r.StatusCode != 409 || body.Holder != "alice" {
		t.Fatalf("second controller = %d %+v", r.StatusCode, body)
	}
	if req.URL.Path = "/v1/term/zzz"; true {
		if r, _ := http.DefaultClient.Do(req); r.StatusCode != 404 {
			t.Fatalf("unknown agent = %d", r.StatusCode)
		}
	}

	c.CloseNow() // the socket drops
	waitFor(t, 2*time.Second, "the lock to release", func() bool { return terms.Holder("a1") == "" })
}
